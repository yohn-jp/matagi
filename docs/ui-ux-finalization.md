# Matagi UI/UX Finalization

This document integrates the production-composition contract for the accepted Matagi presentation design. Issue [#76](https://github.com/yohn-jp/matagi/issues/76) remains the frozen design authority; this summary does not change `docs/architecture.md` or certify later restore/candidate work.

## Product structure and authority

Workspace is the permanent management home for registered environments, services, health, and lifecycle controls. Updates and Settings remain trusted Matagi destinations. Opening an endpoint creates or focuses one presentation view per exact `(environmentID, serviceID, endpointID)` identity. Views may occupy an integrated tab or an independent window; Move/Return preserves the same controller. Closing a view closes presentation only. Main-window close and updater cancellation close every owned controller and window without stopping services or claiming tunnel ownership.

The runtime remains the sole service, desired-state, endpoint, and tunnel authority. UI actions continue through the existing HTTP v1 client. Jinushi owns remote process lifecycle and system OpenSSH remains the transport and authentication primitive. The WebView2 host presents that state; it does not own a second runtime or lifecycle policy.

## Trusted and service surfaces

The trusted Matagi controller owns navigation, Workspace, Updates, Settings, and the 96-DIP two-row header. Each service endpoint has a separate controller below that header or in a detached window. Service pages receive no form token, capability, WebMessage bridge, or host object. The trusted form handler reserves a logical view, ensures the registered endpoint over HTTP v1, validates the response and loopback origin, then asks the composition adapter to create the scoped controller. Native toolbar actions call the same move/close operations as trusted UI forms.

The trusted UI keeps the historical `Matagi/WebView2` user-data folder unchanged. A service controller uses the stable sibling `Matagi/WebView2-services/<sha256>` folder, where the digest is computed from the JSON-encoded structured EndpointKey. Profiles are neither copied nor migrated; no identity, URL, port, or credential is used as a folder name. Registry, locale, update settings, and the `matagi.settings/1` record remain unchanged. Saved desktop state is separate from those records.

## Identity, generations, and endpoint freshness

The presentation model owns only process-local view identity, selection, location, and operation generations. It reserves an endpoint before Ensure so concurrent Opens coalesce. Only the reservation owner ensures. Host `Ready` commits that generation; failure, Close, and newer operations invalidate it. A late callback cannot publish a closed or superseded view, and an accepted host enqueue is not treated as successful controller creation.

Service controllers admit only the exact loopback origin returned by Ensure. The existing state snapshot path is used to invalidate controllers when an endpoint becomes unavailable or its origin changes; a changed origin is closed and requires an explicit fresh Ensure before navigation. Persisted URLs are never navigation authority. A failed or unavailable presenter reports a Matagi error instead of redirecting its trusted controller to service content.

## Movement, closure, and saved-layout boundary

Controller movement uses the integrated Windows host's transactional move/rollback seam. A failed move retains the previous logical location; if native rollback fails, the controller is closed and represented as unavailable. Selecting, moving, or closing views issues no Start, Stop, or Restart operation.

Shutdown first stops presentation commands and invalidates pending generations, then drains `ViewsHost.CloseAll` on the owning STA while its message pump remains active. This uses the existing bounded application shutdown context; no additional timeout is introduced. Only after the host close path does the existing HTTP/runtime teardown continue. Controllers and detached windows are closed through the host; no process-name kill, arbitrary PID cleanup, or tunnel authority is added.

The logical model and bounded desktop store support the later safe-restore contract. This composition does not claim startup restoration or durable layout-event wiring: those remain in Issue #84. Saved layout can never register or admit an endpoint, and restored views require explicit Resume with a fresh Ensure.

## Visual and accessibility contract

Use the accepted Quiet Hunter palette and shared role tokens from #76: restrained mountain green, ink, and iron; hairline structure; no decorative hero, glow, gradient, or ordinary-content shadow. Keep Japanese and English chrome, the existing Updates and Settings semantics, visible keyboard focus, text/non-text contrast requirements, keyboard navigation, and DPI-aware native bounds. Environment/service/endpoint identity stays distinguishable; status is expressed with localized words and shape, not color alone. Product-owned page styling and language are not rewritten.

## Verification boundary

Portable tests establish model-to-adapter generations, endpoint profile identity, and HTTP/UI contracts only. The Windows candidate workflow must run the submitted production executable and its existing WebView2 host/surface shards on the exact PR HEAD. A Windows cross-build is compilation evidence, not proof of runtime behavior. This document makes no real SSH/Jinushi, restored-session, cross-version upgrade, or manual Windows UX certification claim.
