package sentinel_test

// P1.9: the §8 surface, frozen for the 0.4.x line. Renaming or re-typing any
// identifier or field below fails this file to compile. External package
// (sentinel_test) so it also proves an outside importer sees exactly this.

import (
	"context"
	"net/http"
	"time"

	"github.com/rs/zerolog"

	sentinel "github.com/tuju-ai/sentinel/packages/sdk-go"
	"github.com/tuju-ai/sentinel/packages/sdk-go/sentinelzerolog"
)

// Functions and methods core will call (spec §8).
var (
	_ func(sentinel.Config, error, map[string]interface{}, ...sentinel.EventOption) *sentinel.Event = sentinel.NewEvent
	_ func(context.Context, *sentinel.Event)                                                        = sentinel.ApplyContext
	_ func(context.Context) []sentinel.Breadcrumb                                                    = sentinel.BreadcrumbsFromContext
	_ func(context.Context, sentinel.Breadcrumb)                                                     = sentinel.AddBreadcrumb
	_ func(context.Context, int) context.Context                                                     = sentinel.WithBreadcrumbBuffer
	_ func(context.Context) (sentinel.UserInfo, bool)                                                 = sentinel.UserFromContext
	_ func(context.Context) map[string]string                                                        = sentinel.TagsFromContext
	_ func(context.Context) map[string]interface{}                                                   = sentinel.MetadataFromContext
	_ func(context.Context, map[string]interface{}) context.Context                                  = sentinel.WithContextMap
	_ func(context.Context, string) context.Context                                                  = sentinel.WithUser
	_ func(context.Context, sentinel.UserInfo) context.Context                                       = sentinel.WithUserInfo
	_ func(context.Context, string) context.Context                                                  = sentinel.WithTenant
	_ func(context.Context, string, interface{}) context.Context                                     = sentinel.WithTag
	_ func(context.Context, string) context.Context                                                  = sentinel.WithLevel
	_ func(string) string                                                                             = sentinel.NormalizeLevel
	_ func(zerolog.Level) string                                                                      = sentinelzerolog.LevelFromZerolog
	_ func(*sentinel.Event) *sentinel.Event                                                            // (*Config).ApplyBeforeSend, checked below via a value
	_ func() error                                                                                     // (*Config).Validate, checked below
	_ func(sentinel.Config) *sentinel.Transport                                                        = sentinel.NewTransport
	_ func(*sentinel.Transport, *sentinel.Event)                                                       // (*Transport).Push, checked below
	_ func(*sentinel.Transport, time.Duration) bool                                                    // (*Transport).Flush, checked below
	_ func(context.Context) (string, string, uint32)                                                   = sentinel.ExtractTraceContext
)

func init() {
	var cfg sentinel.Config
	var applyBeforeSend func(*sentinel.Event) *sentinel.Event = cfg.ApplyBeforeSend
	_ = applyBeforeSend
	var validate func() error = cfg.Validate
	_ = validate

	var tr *sentinel.Transport
	var push func(*sentinel.Event) = tr.Push
	_ = push
	var flush func(time.Duration) bool = tr.Flush
	_ = flush
}

// Fields and constants core will set or read (spec §8).
var (
	_ string                            = sentinel.Event{}.Level
	_ *sentinel.SDKInfo                 = sentinel.Event{}.SDK
	_ string                            = sentinel.Event{}.ServerName
	_ string                            = sentinel.Event{}.Transaction
	_ *sentinel.UserInfo                = sentinel.Event{}.User
	_ map[string]string                 = sentinel.Event{}.Tags
	_ map[string]map[string]interface{} = sentinel.Event{}.Contexts
	_ *sentinel.RequestInfo             = sentinel.Event{}.Request
	_ []sentinel.Breadcrumb             = sentinel.Event{}.Breadcrumbs

	_ string = sentinel.SDKInfo{}.Name
	_ string = sentinel.SDKInfo{}.Version

	_ string = sentinel.UserInfo{}.ID
	_ string = sentinel.UserInfo{}.Username
	_ string = sentinel.UserInfo{}.Email
	_ string = sentinel.UserInfo{}.IPAddress

	_ string            = sentinel.RequestInfo{}.Method
	_ string            = sentinel.RequestInfo{}.URL
	_ string            = sentinel.RequestInfo{}.QueryString
	_ map[string]string = sentinel.RequestInfo{}.Headers
	_ interface{}       = sentinel.RequestInfo{}.Data
	_ map[string]string = sentinel.RequestInfo{}.Env

	_ time.Time            = sentinel.Breadcrumb{}.Timestamp
	_ string                = sentinel.Breadcrumb{}.Type
	_ string                = sentinel.Breadcrumb{}.Category
	_ string                = sentinel.Breadcrumb{}.Level
	_ string                = sentinel.Breadcrumb{}.Message
	_ map[string]interface{} = sentinel.Breadcrumb{}.Data

	_ string                               = sentinel.Config{}.ServerName
	_ func(*sentinel.Event) *sentinel.Event = sentinel.Config{}.BeforeSend
	_ int                                   = sentinel.Config{}.MaxBreadcrumbs

	_ string = sentinel.LevelFatal
	_ string = sentinel.LevelError
	_ string = sentinel.LevelWarning
	_ string = sentinel.LevelInfo
	_ string = sentinel.LevelDebug
	_ string = sentinel.SDKName
	_ string = sentinel.SDKVersion
	_ int    = sentinel.DefaultMaxBreadcrumbs
	_ int    = sentinel.MaxBreadcrumbsLimit
)

// Today's other exported functions, unchanged (spec §8, §9).
var (
	_ func(error, ...map[string]interface{})                                        = sentinel.CaptureError
	_ func(context.Context, error, ...map[string]interface{})                       = sentinel.CaptureErrorContext
	_ func(sentinel.Config) *sentinel.Client                                        = sentinel.Init
	_ func(time.Duration) bool                                                      = sentinel.Flush
	_ func(sentinel.MiddlewareOptions) func(http.Handler) http.Handler              = sentinel.NewHTTPMiddleware
	_ func(http.Handler) http.Handler                                               = sentinel.HTTPMiddleware
)
