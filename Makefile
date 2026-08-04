.PHONY: build test test-integration-postgres test-phase0 vet check

build:
	go build ./...

test:
	go test ./...

test-integration-postgres:
	@test -f .env.test || (echo ".env.test is required; copy .env.test.example and configure it" && exit 1)
	@set -a; . ./.env.test; set +a; GOCACHE=$${AGENTOPS_TEST_GOCACHE:-/tmp/agentops-go-cache} go test $(TEST_FLAGS) ./test/integration/... -count=1

test-phase0:
	@test -f .env.test || (echo ".env.test is required; copy .env.test.example and configure it" && exit 1)
	@set -a; . ./.env.test; set +a; GOCACHE=$${AGENTOPS_TEST_GOCACHE:-/tmp/agentops-go-cache} go test $(TEST_FLAGS) ./... -count=1

vet:
	go vet ./...

check: build test vet
