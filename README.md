# cozypkg

Cozy wrapper around Helm and Flux CD for local development

## Usage

```
Cozy wrapper around Helm and Flux CD for local development

Usage:
  cozypkg [command]

Available Commands:
  apply       Upgrade or install HelmRelease and sync status
  completion  Generate the autocompletion script for the specified shell
  delete      Uninstall the Helm release
  diff        Show a diff between live and desired manifests
  get         Get one or many HelmReleases
  help        Help about any command
  list        List HelmReleases
  reconcile   Trigger HelmRelease reconciliation (optionally its HelmChart)
  resume      Resume Flux HelmRelease
  show        Render manifests like helm template
  suspend     Suspend Flux HelmRelease
  version     Print version

Flags:
  -h, --help                help for cozypkg
      --kubeconfig string   Path to kubeconfig
  -n, --namespace string    Kubernetes namespace (defaults to the current context)
  -v, --version             version for cozypkg

Use "cozypkg [command] --help" for more information about a command.
```

## Installation

Download binary from Github [releases page](https://github.com/cozystack/cozypkg/releases/latest)

Or use simple script to install it:
```bash
curl -sSL https://github.com/cozystack/cozypkg/raw/refs/heads/main/hack/install.sh | sh -s
```
