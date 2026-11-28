module github.com/refraction-networking/utls

go 1.26.0

retract (
	v1.4.1 // #218
	v1.4.0 // #218 panic on saveSessionTicket
)

require (
	github.com/andybalholm/brotli v1.3.0
	github.com/klauspost/compress v1.20.5
	golang.org/x/crypto v0.59.0
	golang.org/x/net v0.62.0
	golang.org/x/sys v0.50.0
)

require golang.org/x/text v0.44.0 // indirect
