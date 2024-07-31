LANG=en_US.UTF-8
SHELL=/bin/bash
.SHELLFLAGS=--norc --noprofile -e -u -o pipefail -c

run:
	source .env && go run main.go;

build:
	go build -o bin/odesair main.go;
