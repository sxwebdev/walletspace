start:
	go run ./cmd/walletspace

fmt:
	go fix ./...
	gofumpt -l -w .
