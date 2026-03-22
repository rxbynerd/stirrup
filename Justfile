default: build test

build:
    go build -o stirrup-harness ./harness/cmd/harness
    go build -o stirrup-job ./harness/cmd/job
    go build -o stirrup-eval ./eval/cmd/eval

test:
    go test ./harness/... ./types/... ./eval/...

lint:
    golangci-lint run ./...

proto:
    buf generate

buf-lint:
    buf lint

docker:
    docker build --target harness -t stirrup-harness .

docker-job:
    docker build --target job -t stirrup-job .

clean:
    rm -f stirrup-harness stirrup-job stirrup-eval
