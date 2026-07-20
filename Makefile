.PHONY: test test-go test-agent build build-static docker vet fmt run

# Full acceptance loop run by CI / Claude Code Cloud.
test: vet test-go test-agent
	@echo "ALL TESTS PASSED"

test-go:
	go test ./...

# Guards against accidentally importing a CGO dependency.
test-agent:
	CGO_ENABLED=0 go build -o /dev/null .
	sh test-agent.sh

vet:
	go vet ./...

fmt:
	gofmt -w .

build:
	go build -o how-hot-is-it .

# Same static build the Docker image uses.
build-static:
	CGO_ENABLED=0 go build -ldflags="-s -w" -o how-hot-is-it .

docker:
	docker build -t how-hot-is-it .

run: build
	./how-hot-is-it -config config.json -db howhot.db
