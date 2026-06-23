# vgi-elasticsearch Makefile
#
# A VGI worker (Go) that queries an Elasticsearch/OpenSearch index as a SQL
# table, using Point-In-Time + search_after for consistent, resumable deep
# pagination (the cursor lives in externalized scan state). Targets:
#
#   make build       Build the worker + seeder binaries
#   make test-unit   Run pure-Go unit tests (no cluster needed)
#   make test-live   Run the Go live tests against a dockerized OpenSearch
#   make test-sql    Run the haybarn SQL E2E against a dockerized OpenSearch
#   make test        test-unit + test-live + test-sql
#   make fmt / vet / lint / clean
#
# test-sql / test-live need Docker; test-sql also needs haybarn-unittest on PATH:
#   uv tool install haybarn-unittest
#   export PATH="$$HOME/.local/bin:$$PATH"

WORKER_BIN  := vgi-elasticsearch-worker
SEED_BIN    := seed
WORKER_CMD  := ./cmd/vgi-elasticsearch-worker
SEED_CMD    := ./cmd/seed

WORKER_PATH := $(CURDIR)/$(WORKER_BIN)
SEED_PATH   := $(CURDIR)/$(SEED_BIN)

TEST_DIR     := .
TEST_PATTERN := test/sql/*

# Dockerized OpenSearch (Apache-2.0) — single node, security plugin disabled for
# the test only. Host port 9209 avoids clashing with a local 9200.
OS_IMAGE     := opensearchproject/opensearch:2.17.0
OS_CONTAINER := vgi-es-test
OS_PORT      := 9209
OS_URL       := http://127.0.0.1:$(OS_PORT)
E2E_INDEX    := vgi_es_e2e

.PHONY: build test test-unit test-live test-sql os-up os-down fmt vet lint clean

build:
	go build -o $(WORKER_BIN) $(WORKER_CMD)
	go build -o $(SEED_BIN) $(SEED_CMD)

test: test-unit test-live test-sql

test-unit:
	go test ./...

# Bring up a single-node OpenSearch and wait for it to answer cluster health.
os-up:
	@docker rm -f $(OS_CONTAINER) >/dev/null 2>&1 || true
	@docker run -d --name $(OS_CONTAINER) -p $(OS_PORT):9200 \
		-e "discovery.type=single-node" \
		-e "DISABLE_SECURITY_PLUGIN=true" \
		-e "DISABLE_INSTALL_DEMO_CONFIG=true" \
		-e "OPENSEARCH_JAVA_OPTS=-Xms512m -Xmx512m" \
		$(OS_IMAGE) >/dev/null
	@echo "waiting for OpenSearch on $(OS_URL) ..."
	@for i in $$(seq 1 60); do \
		if curl -fs $(OS_URL)/_cluster/health >/dev/null 2>&1; then \
			echo "OpenSearch is up"; exit 0; \
		fi; \
		sleep 2; \
	done; \
	echo "ERROR: OpenSearch did not become ready" >&2; \
	docker logs --tail 30 $(OS_CONTAINER) >&2; exit 1

os-down:
	@docker rm -f $(OS_CONTAINER) >/dev/null 2>&1 || true

# Go live tests (PIT/search_after resume, projection, predicate, type mapping)
# against the dockerized OpenSearch.
test-live: build
	@set -e; \
	$(MAKE) os-up; \
	trap '$(MAKE) os-down' EXIT; \
	VGI_ES_TEST_URL=$(OS_URL) go test ./internal/esworker/ -run TestLive -v

# haybarn SQL E2E: boot OpenSearch, seed the fixed index, run the suite, tear down.
test-sql: build
	@set -e; \
	$(MAKE) os-up; \
	trap '$(MAKE) os-down' EXIT; \
	$(SEED_PATH) --url $(OS_URL) --index $(E2E_INDEX) --count 25; \
	VGI_ELASTICSEARCH_WORKER="$(WORKER_PATH)" \
	VGI_ES_TEST_URL="$(OS_URL)" \
	VGI_ES_TEST_INDEX="$(E2E_INDEX)" \
		haybarn-unittest --test-dir "$(TEST_DIR)" "$(TEST_PATTERN)"

fmt:
	gofmt -w .

vet:
	go vet ./...

lint:
	@if command -v golangci-lint >/dev/null 2>&1; then \
		golangci-lint run ./...; \
	else \
		echo "golangci-lint not found; running go vet instead"; \
		go vet ./...; \
	fi

clean:
	rm -f $(WORKER_BIN) $(SEED_BIN)
