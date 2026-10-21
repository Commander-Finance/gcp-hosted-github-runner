# Go 1.26.10 is the first 1.26 release whose crypto/x509 rejects chains through
# the compromised eMudhra intermediates; the autoscaler sends its GitHub token
# through that verifier. Keep this in step with the toolchain line in go.mod.
FROM --platform=$BUILDPLATFORM golang:1.26.10-bookworm@sha256:ee3901c61e423732605a51f1b8157c849ec01194ea0a274213b5202b2c11e7bf AS build

# The runtime image is scratch and copies this stage's CA bundle. Bookworm's
# ca-certificates before 20250419~deb12u2 (DLA-4845-1) still trusts emSign ECC
# Root CA - G3, the root above the compromised intermediates, so upgrade and fail
# the build if the patched package is not what installed.
ARG MIN_CA_CERTIFICATES=20250419~deb12u2
RUN apt-get -o Acquire::Retries=3 update && \
    apt-get -o Acquire::Retries=3 install -y --only-upgrade ca-certificates && \
    dpkg --compare-versions "$(dpkg-query -W -f='${Version}' ca-certificates)" ge "$MIN_CA_CERTIFICATES" && \
    rm -rf /var/lib/apt/lists/*

WORKDIR /build
COPY ./go.mod ./
COPY ./go.sum ./
COPY ./pkg ./pkg
COPY main.go ./
RUN go mod download

# The image pins GOTOOLCHAIN=local, so the FROM line alone decides which
# crypto/x509 ships. Run the distrust test on that toolchain so a FROM that
# drifts to an unfixed Go fails the build instead of shipping.
RUN go test -count=1 -run '^TestVerifyRejectsCompromisedEmudhraChain$' ./pkg

ARG UID=11010 GID=11010
RUN echo "scaler:*:$UID:$GID::/:" > passwd && echo "scaler:*:$GID:" > group

ARG TARGETOS TARGETARCH
RUN GOOS=$TARGETOS GOARCH=$TARGETARCH CGO_ENABLED=0 go build -a -tags netgo,osusergo -trimpath -ldflags '-w -s -buildid= -extldflags "-static"' -o autoscaler main.go


FROM scratch AS run
LABEL org.opencontainers.image.title="github-runner-autoscaler"
LABEL org.opencontainers.image.authors="Björn Stresing <bjoern.stresing@gmx.de>"
LABEL org.opencontainers.image.description="This image provides a webserver that listens for GitHub actions webhooks and provision compute instances if needed"
LABEL org.opencontainers.image.source="https://github.com/Privatehive/gcp-hosted-github-runner"
LABEL org.opencontainers.image.url="https://github.com/users/Privatehive/packages/container/package/runner-autoscaler"

WORKDIR /rundir
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=build /build/passwd /build/group /etc/
COPY --from=build /build/autoscaler ./
ENV GIN_MODE=release
USER scaler:scaler
EXPOSE 8080/tcp
ENTRYPOINT ["/rundir/autoscaler"]
