# Scoped credential rotation recovery

The staged framework uses eight-hour TokenRequest credentials refreshed by
`hybrid-training-credentials.timer` every four hours. A failed Member API read
previously left the service failed until its next timer activation. That can
leave controllers without valid API credentials. Process readiness alone does
not establish an authenticated, reconciling controller.

Install `config/operations/credential-rotation-retry.conf` as
`/etc/systemd/system/hybrid-training-credentials.service.d/retry.conf`, reload
systemd and run the existing credential service. The drop-in retries failures
after 60 seconds without changing its RBAC, credentials' lifetime, or target
namespace. A successful service invocation still follows the four-hour timer.

The kubeconfigs must use `tokenFile` and mount the whole credential Secret.
Clients that started with older inline-token kubeconfigs need one rollout to
load the tokenFile configuration. Do not store or log token contents.

Before starting paid work, verify the rotation service's successful receipt,
fresh renewTime for every controller leader Lease, and a new controller-owned
observation. Secret projection and client refresh are asynchronous; do not
equate a successful Secret update with instant controller recovery. Preserve
failed rotation records. Stop new paid work if authenticated reconciliation
cannot be established.
