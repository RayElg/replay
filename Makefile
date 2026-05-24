.PHONY: dev down logs migrate example test clean build

# Bring up all services (rebuild containers)
dev:
	docker compose up --build -d
	@echo ""
	@echo "  ✔ Replay is running"
	@echo "  UI:            http://localhost:3000"
	@echo "  API:           http://localhost:8080"
	@echo "  MinIO Console: http://localhost:9091"
	@echo "  PostgreSQL:    localhost:5432"
	@echo "  MQTT:          localhost:1883"
	@echo ""

# Tear down all services
down:
	docker compose down

# Follow logs for all services
logs:
	docker compose logs -f

# Run migrations (via control-plane).
# We start a one-shot container with --migrate-only rather than `exec` into the
# running one — `exec` would try to bind :8080 a second time inside the live
# container and fail. The flag matches the long-form in main.go.
migrate:
	docker compose run --rm --no-deps control-plane --migrate-only

# Seed a self-contained example test (one pass, one fail) against example.com
# and queue a run. Needs `make dev` first so the runner is up to execute it.
example:
	docker compose run --rm --no-deps control-plane seed-example --run

# Run tests
test:
	cd cmd/control-plane && go test ./...
	cd cmd/runner && go test ./...
	cd internal/replaycrypto && go test ./...

# Clean up everything: containers, volumes, build cache
clean:
	docker compose down -v --remove-orphans
	docker system prune -f

# Build Go binaries locally (not in Docker)
build:
	cd cmd/control-plane && go build -o ../../bin/control-plane .
	cd cmd/runner && go build -o ../../bin/runner .
