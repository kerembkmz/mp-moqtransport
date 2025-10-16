module multipath-moq

go 1.23.6

replace github.com/mengelbart/moqtransport => ../../

require (
	github.com/mengelbart/moqtransport v0.0.0
	github.com/quic-go/quic-go v0.53.0
)

require (
	github.com/mengelbart/qlog v0.1.0 // indirect
	go.uber.org/mock v0.5.0 // indirect
	golang.org/x/crypto v0.36.0 // indirect
	golang.org/x/mod v0.22.0 // indirect
	golang.org/x/net v0.38.0 // indirect
	golang.org/x/sync v0.12.0 // indirect
	golang.org/x/sys v0.31.0 // indirect
	golang.org/x/tools v0.29.0 // indirect
)
