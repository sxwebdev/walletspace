start:
	go run ./cmd/tronfaucet

fmt:
	go fix ./...
	gofumpt -l -w .
