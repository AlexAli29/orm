// The OpenTelemetry adapter is a module of its own so that a project importing
// only the ORM never compiles OpenTelemetry.
//
// The OTel version is pinned rather than floating. The releases from v1.45
// onwards declare go 1.25, and this project's floor is the 1.24 in the core
// module's go.mod — a floating dependency would raise the toolchain a project
// needs as a side effect of an upstream release, which is a decision rather
// than an accident. v1.37 is the newest that still builds on 1.24.
module github.com/AlexAli29/orm/ormotel

go 1.24

require (
	github.com/AlexAli29/orm v0.1.0
	go.opentelemetry.io/otel v1.37.0
	go.opentelemetry.io/otel/sdk v1.37.0
	go.opentelemetry.io/otel/trace v1.37.0
)

require (
	github.com/go-logr/logr v1.4.3 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/pgx/v5 v5.7.2 // indirect
	go.opentelemetry.io/auto/sdk v1.1.0 // indirect
	go.opentelemetry.io/otel/metric v1.37.0 // indirect
	golang.org/x/crypto v0.31.0 // indirect
	golang.org/x/sys v0.33.0 // indirect
	golang.org/x/text v0.21.0 // indirect
)

