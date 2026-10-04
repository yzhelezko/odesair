LANG=en_US.UTF-8
SHELL=/bin/bash
.SHELLFLAGS=--norc --noprofile -e -u -o pipefail -c

.PHONY: run build test bench live login

run:
	source .env && go run .

build:
	go build -o bin/odesair .

test:
	go vet ./...
	go test -race ./...

bench:
	go test -run '^$$' -bench . -benchmem ./...

live:
	source .env && LIVE_LLM=1 go test -run TestLiveLLM -v -count=1 .

login:
	go run . login
