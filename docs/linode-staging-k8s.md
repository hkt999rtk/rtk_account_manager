# Linode Staging K8s Runtime

Linode staging runtime is K8s-only. The previous Account Manager Linode VM
toolkit has been retired and must not be used for staging provision, deploy,
verify, backup, or VM log collection.

Operate staging from the workspace root:

```sh
./stg.sh provision --confirm video-cloud-staging
scripts/run-staging-e2e.sh --confirm video-cloud-staging
```

Account Manager staging endpoints and bootstrap credentials are provided through
the workspace K8s service discovery and runtime secret flow.
