SHELL := /bin/bash
.DEFAULT_GOAL := help
NS := quizbox
APPS := gateway frontend account quiz game redis

.PHONY: e2e kill-pod help cluster-server cluster-agent db-up db-down images secrets render deploy deploy-ha \
        prepull where watch undeploy dev dev-stop test web

help: ## Show this help
	@awk 'BEGIN{FS=":.*## "} /^[a-zA-Z_-]+:.*## /{printf "  \033[1m%-15s\033[0m %s\n",$$1,$$2}' $(MAKEFILE_LIST)

cluster-server: ## Install k3s server on this VM (IFACE=ens18 optional; needs sudo)
	sudo -E cluster/server.sh

cluster-agent: ## Join this Pi as agent: make cluster-agent K3S_URL=https://<vm>:6443 K3S_TOKEN=<token>
	@test -n "$(K3S_URL)" -a -n "$(K3S_TOKEN)" || { echo "usage: make cluster-agent K3S_URL=https://<vm-ip>:6443 K3S_TOKEN=<token>"; exit 1; }
	sudo -E cluster/agent.sh "$(K3S_URL)" "$(K3S_TOKEN)"

db-up: ## Start Postgres (Docker) on this VM
	@test -f .env || { echo "create .env first: cp .env.example .env"; exit 1; }
	docker compose --env-file .env -f db/docker-compose.yml up -d

db-down: ## Stop Postgres (data stays in db/pgdata)
	docker compose --env-file .env -f db/docker-compose.yml down

images: ## Build + push all 5 multi-arch images (REGISTRY, TAG from .env)
	scripts/images.sh

secrets: ## Create/update Secret quizbox-db from .env
	scripts/secrets.sh

render: ## Generate deploy/generated from .env (MODE=base|ha)
	scripts/render.sh $(or $(MODE),base)

deploy: secrets ## Render + apply base (1 replica each) and wait for rollout
	scripts/render.sh base
	kubectl apply -k deploy/generated
	@for d in $(APPS); do kubectl -n $(NS) rollout status deployment/$$d --timeout=180s || exit 1; done

deploy-ha: secrets ## Same as deploy, but 2 replicas of the five stateless services
	scripts/render.sh ha
	kubectl apply -k deploy/generated
	@for d in $(APPS); do kubectl -n $(NS) rollout status deployment/$$d --timeout=180s || exit 1; done

prepull: ## Pre-pull all images on every agent (DaemonSet)
	scripts/render.sh $(or $(MODE),base)
	kubectl create namespace $(NS) --dry-run=client -o yaml | kubectl apply -f -
	kubectl apply -k deploy/generated/prepull
	kubectl -n $(NS) rollout status daemonset/prepull --timeout=300s

where: ## Which pod runs on which node
	@kubectl get pods -n $(NS) --sort-by=.spec.nodeName \
	  -o custom-columns=NAME:.metadata.name,NODE:.spec.nodeName,STATUS:.status.phase

watch: ## Watch pods (wide)
	kubectl get pods -n $(NS) -o wide -w

undeploy: ## Delete everything in the quizbox namespace (incl. namespace and secret)
	-kubectl delete -k deploy/generated/prepull --ignore-not-found
	kubectl delete -k deploy/generated --ignore-not-found
	-kubectl delete namespace $(NS) --ignore-not-found

dev: ## Start Postgres+Redis in Docker, then all services natively (scripts/dev.sh)
	docker compose -f docker-compose.dev.yml up -d --wait postgres redis
	scripts/dev.sh

dev-stop: ## Stop native services (add: docker compose -f docker-compose.dev.yml down)
	scripts/dev.sh stop

test: ## go test ./...
	go test ./...

web: ## Build the frontend into services/frontend/dist/index.html
	cd frontend/web && npm ci && npm run build

BASE_URL ?= http://192.168.0.10:30080
e2e: ## End-to-end tests against a running stack (BASE_URL=http://192.168.0.10:30080 or http://localhost:8080)
	BASE_URL=$(BASE_URL) go test -tags e2e -count=1 -v ./test/e2e/

kill-pod: ## Rehearsal: delete one service's pod and time its replacement (SVC=quiz)
	@test -n "$(SVC)" || { echo "usage: make kill-pod SVC=<gateway|frontend|account|quiz|game|redis>"; exit 1; }
	@start=$$(date +%s); kubectl -n quizbox delete pod -l app=$(SVC) --wait=false; \
	kubectl -n quizbox rollout status deploy/$(SVC) --timeout=120s; \
	echo "$(SVC) back after $$(( $$(date +%s) - start ))s"; \
	kubectl -n quizbox get pods -l app=$(SVC) -o wide
