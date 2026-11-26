package e2e

// Config is loaded from environment variables at suite startup.
// See test/framework/framework.go for the variable names and defaults.
//
// Required for cluster tests:
//   E2E_KUBECONFIG     path to kubeconfig (defaults to $KUBECONFIG)
//   E2E_STORAGE_CLASS  StorageClass name  (default: niova-csi-sc)
//   E2E_NAMESPACE      test namespace     (default: niova-csi-test)
//   E2E_NODE_NAME      target node        (required for node-level tests)
//   E2E_FIO_IMAGE      fio container image (default: ljishen/fio:latest)
