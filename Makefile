.PHONY: test test-go test-agent build build-static docker vet fmt run

# The Go module lives under server/; the agent is a root-level shell script.
SERVER := server

# Full acceptance loop run by CI / Claude Code Cloud.
test: vet test-go test-agent
	@echo "ALL TESTS PASSED"

test-go:
	cd $(SERVER) && go test ./...

# Guards against accidentally importing a CGO dependency.
test-agent:
	cd $(SERVER) && CGO_ENABLED=0 go build -o /dev/null .
	sh test-agent.sh

vet:
	cd $(SERVER) && go vet ./...

fmt:
	cd $(SERVER) && gofmt -w .

build:
	cd $(SERVER) && go build -o ../how-hot-is-it .

# Same static build the Docker image uses.
build-static:
	cd $(SERVER) && CGO_ENABLED=0 go build -ldflags="-s -w" -o ../how-hot-is-it .

docker:
	docker build ./$(SERVER)

run: build
	./how-hot-is-it -config config.json -db howhot.db
