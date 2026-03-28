.PHONY: build run podman-build podman-push helm-deploy

build:
	go build ./...

run:
	./maas-tokenizer

podman-build:
	podman build $(PODMAN_ARGS) -f Containerfile -t quay.io/eformat/maas-tokenizer:latest .

podman-push:
	podman push quay.io/eformat/maas-tokenizer:latest

helm-deploy:
	helm upgrade --install maas-tokenizer ./chart $(HELM_ARGS)
