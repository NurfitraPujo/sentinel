package sentrycompat

import "net/http"

// The four patterns of the Sentry-compatible routes (spec §3.1). {$} anchors the slash form, so
// /api/1/envelope/anything is a 404 and not the envelope handler; the slash-less form is registered explicitly so
// the mux never answers a POST with its 307 to the slash form; the method prefix makes the mux itself answer every
// other method with 405 and Allow: POST, before any chain runs.
const (
	PatternEnvelope        = "POST /api/{project_id}/envelope/{$}"
	PatternEnvelopeNoSlash = "POST /api/{project_id}/envelope"
	PatternStore           = "POST /api/{project_id}/store/{$}"
	PatternStoreNoSlash    = "POST /api/{project_id}/store"
	PathValueProjectID     = "project_id"
)

// Mount registers envelope and store on mux, each under both spellings of its path. envelope and store are the
// FULL chains (otelhttp outermost); Mount adds nothing around them.
func Mount(mux *http.ServeMux, envelope, store http.Handler) {
	mux.Handle(PatternEnvelope, envelope)
	mux.Handle(PatternEnvelopeNoSlash, envelope)
	mux.Handle(PatternStore, store)
	mux.Handle(PatternStoreNoSlash, store)
}
