.PHONY: helm-test helm-e2e build web test itest itest-k8s itest-db itest-git itest-registry itest-app e2e lint run up down demo

web: ## build the web interface into web/dist (embedded by the next `go build`)
	cd web && npm ci && npm run build
	touch web/dist/.gitkeep

build: web
	go build -o bin/plinth ./cmd/plinth

test:
	go test -race ./...
	cd web && npm test --silent

itest: itest-app itest-k8s itest-db itest-git itest-registry ## all integration tests (need Docker)

itest-k8s: ## Kubernetes driver on a throwaway kind/k3d cluster
	./scripts/itest.sh

itest-db: ## Postgres driver on disposable containers (PLINTH_TEST_PG_VERSIONS=17,16,15)
	go test -tags=integration -race -count=1 ./internal/provider/postgres/...

itest-git: ## Gitea driver on a disposable container (needs git; PLINTH_TEST_GITEA_VERSIONS=1.23)
	go test -tags=integration -race -count=1 ./internal/provider/gitea/...

itest-registry: ## Harbor driver on a disposable Harbor (slow; or set PLINTH_TEST_HARBOR_URL/USER/PASSWORD)
	go test -tags=integration -race -count=1 -timeout 25m ./internal/provider/harbor/...

e2e: ## whole request through all real drivers on a disposable stack (slow; needs Docker, k3d, kubectl, git)
	./scripts/e2e.sh

itest-app: ## whole application (API, store, auth, worker) on a disposable PostgreSQL, fake drivers
	go test -tags=integration -race -count=1 ./internal/api/...

helm-test: ## lint and render the Helm chart with many value sets (needs helm)
	helm lint deploy/helm/plinth -f deploy/helm/plinth/ci/minimal-values.yaml
	helm lint deploy/helm/plinth -f deploy/helm/plinth/ci/full-values.yaml
	go test -count=1 ./deploy/helm/

helm-e2e: ## install the chart on a throwaway k3d cluster and test it (slow; needs Docker, k3d, kubectl, helm)
	./scripts/helm-e2e.sh

lint:
	go vet ./...
	@test -z "$$(gofmt -l .)" || (gofmt -l .; exit 1)

demo: web ## try the web interface with simulated systems (needs Docker for PostgreSQL)
	docker compose up -d --wait postgres
	go run ./cmd/plinth-demo

up:
	docker compose up -d postgres

down:
	docker compose down

run: build
	set -a; . ./.env; set +a; ./bin/plinth
