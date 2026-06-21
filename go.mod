module github.com/hstern/go-mailbox-720

go 1.26.4

require (
	github.com/CiscoM31/godata v1.0.11
	github.com/coreos/go-oidc/v3 v3.19.0
	github.com/emersion/go-ical v0.0.0-20250609112844-439c63cef608
	github.com/emersion/go-imap/v2 v2.0.0-beta.8
	github.com/emersion/go-message v0.18.2
	github.com/emersion/go-sasl v0.0.0-20241020182733-b788ff22d5a6
	github.com/emersion/go-smtp v0.24.0
	github.com/emersion/go-vcard v0.0.0-20260618161152-d854b7e0e2d3
	github.com/emersion/go-webdav v0.7.1-0.20260411103855-046391163625 // master: caldav.SyncCollection (added post-v0.7.0); pin a tag once one ships
	github.com/go-faster/errors v0.7.1
	github.com/go-faster/jx v1.2.0
	github.com/go-jose/go-jose/v4 v4.1.4
	github.com/hstern/go-subjectid v0.0.0-20260525222327-b47140763585
	github.com/ogen-go/ogen v1.22.0
	go.opentelemetry.io/otel v1.44.0
	go.opentelemetry.io/otel/metric v1.44.0
	go.opentelemetry.io/otel/trace v1.44.0
	gopkg.in/yaml.v3 v3.0.1
)

require (
	git.sr.ht/~rockorager/go-jmap v0.5.3
	github.com/hstern/go-access-tokens v0.3.0
	github.com/hstern/go-bearer-token v0.2.0
	github.com/hstern/go-caep v0.1.0
	github.com/hstern/go-jscontact v0.1.0
	github.com/hstern/go-protected-resource-metadata v0.1.0
	github.com/hstern/go-risc v0.2.0
	github.com/hstern/go-secevent v0.1.0
	github.com/hstern/go-ssf v0.1.0
	github.com/hstern/go-token-introspection v0.1.0
	github.com/teambition/rrule-go v1.8.2
)

require (
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/dlclark/regexp2 v1.12.0 // indirect
	github.com/fatih/color v1.19.0 // indirect
	github.com/ghodss/yaml v1.0.0 // indirect
	github.com/go-faster/yaml v0.4.6 // indirect
	github.com/go-logr/logr v1.4.3 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/hstern/go-jscalendar v0.2.0 // indirect
	github.com/mattn/go-colorable v0.1.14 // indirect
	github.com/mattn/go-isatty v0.0.22 // indirect
	github.com/segmentio/asm v1.2.1 // indirect
	github.com/shopspring/decimal v1.4.0 // indirect
	go.opentelemetry.io/auto/sdk v1.2.1 // indirect
	go.uber.org/multierr v1.11.0 // indirect
	go.uber.org/zap v1.28.0 // indirect
	golang.org/x/exp v0.0.0-20230725093048-515e97ebf090 // indirect
	golang.org/x/net v0.56.0 // indirect
	golang.org/x/oauth2 v0.36.0 // indirect
	golang.org/x/sync v0.21.0 // indirect
	golang.org/x/sys v0.46.0 // indirect
	golang.org/x/text v0.38.0 // indirect
	gopkg.in/yaml.v2 v2.4.0 // indirect
)

// Fork adds QRESYNC VANISHED client support (the mail delta uses it for deletion
// tombstones); drop this once upstream PR emersion/go-imap#757 merges and ships.
replace github.com/emersion/go-imap/v2 => github.com/hstern/go-imap/v2 v2.0.0-beta.8.0.20260620025710-a2d23bc67297
