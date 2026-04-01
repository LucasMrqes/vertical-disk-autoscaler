IMG ?= vertical-disk-autoscaler:latest
CHART_DIR ?= charts/vertical-disk-autoscaler
NAMESPACE ?= disk-autoscaler-system
RELEASE ?= disk-autoscaler

CONTROLLER_GEN ?= $(shell which controller-gen 2>/dev/null || echo $(GOBIN)/controller-gen)
GOBIN ?= $(shell go env GOPATH)/bin

.PHONY: build test fmt vet docker-build docker-push install uninstall helm-lint generate manifests controller-gen

build:
	CGO_ENABLED=0 go build -o bin/manager ./cmd/main.go

test:
	CGO_ENABLED=0 go test ./... -v

fmt:
	go fmt ./...

vet:
	CGO_ENABLED=0 go vet ./...

docker-build:
	docker build -t $(IMG) .

docker-push:
	docker push $(IMG)

generate: controller-gen
	$(CONTROLLER_GEN) object paths="./api/..."

manifests: controller-gen
	$(CONTROLLER_GEN) crd:allowDangerousTypes=true paths="./api/..." output:crd:artifacts:config=$(CHART_DIR)/crds

controller-gen:
	@which controller-gen > /dev/null 2>&1 || go install sigs.k8s.io/controller-tools/cmd/controller-gen@latest

helm-lint:
	helm lint $(CHART_DIR)

install:
	helm upgrade --install $(RELEASE) $(CHART_DIR) \
		--namespace $(NAMESPACE) --create-namespace \
		--set image.repository=$(IMG)

uninstall:
	helm uninstall $(RELEASE) --namespace $(NAMESPACE)
