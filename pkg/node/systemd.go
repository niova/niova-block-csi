package node

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/coreos/go-systemd/v22/dbus"
	godbus "github.com/godbus/dbus/v5"
	"k8s.io/klog/v2"
)

const ublkSystemdSlice = "niova-ublk.slice"

// ublkUnitName derives a deterministic systemd unit name from the volume
// ID, so start/stop never need to remember state across a node-plugin
// restart — NodeUnstageVolume always hands the volumeID back, which is
// enough to recompute the unit to stop.
func ublkUnitName(volumeID string) string {
	return fmt.Sprintf("niova-ublk-%s.service", volumeID)
}

// startUblkUnit asks the host's systemd to fork/exec niova-ublk as a
// transient service. Because systemd — not this container — does the
// forking, the process is never a child of the node-plugin's process
// tree: restarting the node-plugin container (crash, OOM, image upgrade)
// does not affect it. Returns the daemon's PID.
func startUblkUnit(volumeID, binary string, args []string, env []string, dir string) (int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	conn, err := dbus.NewSystemdConnectionContext(ctx)
	if err != nil {
		return -1, fmt.Errorf("failed to connect to systemd: %v", err)
	}
	defer conn.Close()

	unitName := ublkUnitName(volumeID)
	execArgs := append([]string{binary}, args...)

	// Best-effort: a prior instance of this unit that exhausted
	// StartLimitBurst (see Restart= below) is left in `failed` state by
	// systemd until explicitly acknowledged -- StartTransientUnit then
	// refuses to reuse the name ("already loaded or has a fragment
	// file"), permanently blocking this volume from ever starting again.
	// A unit that isn't failed (or doesn't exist yet) makes this a no-op.
	_ = conn.ResetFailedUnitContext(ctx, unitName)

	props := []dbus.Property{
		dbus.PropDescription(fmt.Sprintf("niova-ublk daemon for volume %s", volumeID)),
		dbus.PropType("simple"),
		dbus.PropExecStart(execArgs, true),
		dbus.PropSlice(ublkSystemdSlice),
		{Name: "Environment", Value: godbus.MakeVariant(env)},
		{Name: "WorkingDirectory", Value: godbus.MakeVariant(dir)},
		// Bring a crashed daemon back on its own: with niova-ublk run
		// with -r (UBLK_F_USER_RECOVERY), the kernel keeps the ublk
		// device quiesced-and-waiting across a daemon death instead of
		// tearing it down, so a respawn is what actually lets the
		// volume recover instead of hanging forever. Rate-limited so a
		// persistently-crashing daemon doesn't loop forever.
		{Name: "Restart", Value: godbus.MakeVariant("on-failure")},
		{Name: "RestartUSec", Value: godbus.MakeVariant(uint64(2 * time.Second / time.Microsecond))},
		{Name: "StartLimitIntervalUSec", Value: godbus.MakeVariant(uint64(5 * time.Minute / time.Microsecond))},
		{Name: "StartLimitBurst", Value: godbus.MakeVariant(uint32(5))},
		// Equivalent to `systemd-run --collect`: automatically unload this
		// transient unit once it goes inactive or failed, so a unit that
		// exhausts StartLimitBurst doesn't get stuck in `failed` state and
		// block future StartTransientUnitContext calls for this volumeID.
		{Name: "CollectMode", Value: godbus.MakeVariant("inactive-or-failed")},
	}

	resultChan := make(chan string, 1)
	if _, err := conn.StartTransientUnitContext(ctx, unitName, "replace", props, resultChan); err != nil {
		return -1, fmt.Errorf("failed to start transient unit %s: %v", unitName, err)
	}

	select {
	case result := <-resultChan:
		if result != "done" {
			return -1, fmt.Errorf("systemd job for unit %s finished with result %q", unitName, result)
		}
	case <-ctx.Done():
		return -1, fmt.Errorf("timed out waiting for systemd to start unit %s", unitName)
	}

	prop, err := conn.GetServicePropertyContext(ctx, unitName, "MainPID")
	if err != nil {
		return -1, fmt.Errorf("failed to get MainPID for unit %s: %v", unitName, err)
	}
	pid, ok := prop.Value.Value().(uint32)
	if !ok || pid == 0 {
		return -1, fmt.Errorf("unit %s has no live MainPID after start", unitName)
	}

	klog.Infof("Started niova-ublk as systemd unit %s (pid %d) for volume %s", unitName, pid, volumeID)
	return int(pid), nil
}

// runHostCommand runs a short-lived command to completion as a transient
// oneshot systemd unit on the host, the same way startUblkUnit runs
// niova-ublk itself. Needed for host-only tools like the ublk CLI: the
// node-plugin container doesn't reliably have a working copy of their
// runtime (shared libraries staged into the image can drift or fail to
// resolve there), while the host environment niova-ublk already runs in
// does. unitSuffix must be unique per logical caller (e.g. volumeID) so
// concurrent/repeated calls don't collide on the same transient unit name.
func runHostCommand(unitSuffix, binary string, args []string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	conn, err := dbus.NewSystemdConnectionContext(ctx)
	if err != nil {
		return fmt.Errorf("failed to connect to systemd: %v", err)
	}
	defer conn.Close()

	unitName := fmt.Sprintf("niova-run-%s.service", unitSuffix)
	execArgs := append([]string{binary}, args...)

	props := []dbus.Property{
		dbus.PropDescription(fmt.Sprintf("%s %v", binary, args)),
		dbus.PropType("oneshot"),
		dbus.PropExecStart(execArgs, true),
		// Equivalent to `systemd-run --collect`.
		{Name: "CollectMode", Value: godbus.MakeVariant("inactive-or-failed")},
	}

	resultChan := make(chan string, 1)
	if _, err := conn.StartTransientUnitContext(ctx, unitName, "replace", props, resultChan); err != nil {
		return fmt.Errorf("failed to start transient unit %s: %v", unitName, err)
	}

	var result string
	select {
	case result = <-resultChan:
	case <-ctx.Done():
		return fmt.Errorf("timed out waiting for %s to run %s %v", unitName, binary, args)
	}

	if result != "done" {
		detail := ""
		if prop, perr := conn.GetServicePropertyContext(ctx, unitName, "ExecMainStatus"); perr == nil {
			detail = fmt.Sprintf(" (ExecMainStatus=%v)", prop.Value.Value())
		}
		_ = conn.ResetFailedUnitContext(ctx, unitName)
		return fmt.Errorf("%s %v failed on host: job result %q%s", binary, args, result, detail)
	}

	klog.Infof("Ran %s %v on host via unit %s", binary, args, unitName)
	return nil
}

// stopUblkUnit stops the systemd unit for volumeID, if it exists. systemd
// sends SIGTERM, escalates to SIGKILL on timeout, and cleans up the
// transient unit and its cgroup — no PID bookkeeping required on our
// side, and this works even if the node plugin restarted in between,
// since the unit name is recomputed from volumeID rather than remembered.
func stopUblkUnit(volumeID string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	conn, err := dbus.NewSystemdConnectionContext(ctx)
	if err != nil {
		return fmt.Errorf("failed to connect to systemd: %v", err)
	}
	defer conn.Close()

	unitName := ublkUnitName(volumeID)
	resultChan := make(chan string, 1)
	if _, err := conn.StopUnitContext(ctx, unitName, "replace", resultChan); err != nil {
		var dbusErr godbus.Error
		if errors.As(err, &dbusErr) && dbusErr.Name == "org.freedesktop.systemd1.NoSuchUnit" {
			klog.Infof("systemd unit %s not found, considering it already stopped", unitName)
			return nil
		}
		return fmt.Errorf("failed to stop unit %s: %v", unitName, err)
	}

	select {
	case result := <-resultChan:
		if result != "done" {
			return fmt.Errorf("systemd job to stop unit %s finished with result %q", unitName, result)
		}
	case <-ctx.Done():
		return fmt.Errorf("timed out waiting for systemd to stop unit %s", unitName)
	}

	klog.Infof("Stopped systemd unit %s", unitName)
	return nil
}
