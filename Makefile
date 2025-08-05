.PHONY: test test-race bench build-cli

test:
	go test ./...

test-race:
	go test -race ./tests/ ./lsm/...

bench:
	go test -bench=. -benchtime=2s -timeout=30m ./bench/

build-cli:
	go build -o kvstore ./cli/
