# Use distroless as minimal base image to package the daemon binary
# Refer to https://github.com/GoogleContainerTools/distroless for more details
FROM gcr.io/distroless/static:nonroot
ARG TARGETARCH
WORKDIR /
COPY ./dist/krci-env-daemon-${TARGETARCH} /krci-env-daemon

USER 65532:65532

ENTRYPOINT ["/krci-env-daemon"]
