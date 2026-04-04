default: build test

build:
    go build -o stirrup ./harness/cmd/stirrup
    go build -o stirrup-eval ./eval/cmd/eval

test:
    go test ./harness/... ./types/... ./eval/...

lint:
    golangci-lint run ./harness/... ./types/... ./eval/...

proto:
    buf generate

buf-lint:
    buf lint

docker:
    docker build -t stirrup .

clean:
    rm -f stirrup stirrup-eval
