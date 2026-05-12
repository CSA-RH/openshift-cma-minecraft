# Convenience targets for the mc-waker container build.
#
# Override IMG / NAMESPACE on the command line:
#   make image IMG=quay.io/me/mc-waker:v0.1.0
#   make push  IMG=quay.io/me/mc-waker:v0.1.0
#   make ocp-build NAMESPACE=minecraft
IMG       ?= mc-waker:dev
NAMESPACE ?= minecraft
ENGINE    ?= podman

.PHONY: tidy test image run push ocp-build clean

tidy:
	go mod tidy

test:
	go test ./...

image:
	$(ENGINE) build -t $(IMG) -f Containerfile .

run:
	$(ENGINE) run --rm -it -p 25565:25565 -p 8080:8080 \
	    -e WAKER_UPSTREAM=host.docker.internal:25566 \
	    $(IMG)

push:
	$(ENGINE) push $(IMG)

# In-cluster build using OpenShift BuildConfig + ImageStream — handy if your
# laptop can't reach an external registry but can reach the cluster.
ocp-build:
	oc -n $(NAMESPACE) new-build --binary --strategy=docker --name=mc-waker \
	    || true
	oc -n $(NAMESPACE) start-build mc-waker --from-dir=. --follow

clean:
	rm -f mc-waker
