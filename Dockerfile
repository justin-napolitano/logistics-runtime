FROM golang:1.27.3-bookworm@sha256:972b4110ba0f47539a975c31e52c64765e1ff19f2d74469ae4a595c6676e80a5 AS build
WORKDIR /src
ENV GOTOOLCHAIN=local CGO_ENABLED=0
COPY go.mod go.sum ./
RUN go mod download && go mod verify
COPY cmd ./cmd
COPY internal ./internal
RUN go build -trimpath -ldflags="-s -w" -o /out/logistics ./cmd/logistics

FROM gcr.io/distroless/static-debian13:nonroot@sha256:18f4cc26c9ffa7f20a359298be7deadac5c607c8e03a02b8119f1a5fc2f82c99
ARG REVISION=unknown
LABEL org.opencontainers.image.source="https://github.com/justin-napolitano/logistics-runtime" \
      org.opencontainers.image.revision=$REVISION
COPY --from=build --chown=65532:65532 /out/logistics /logistics
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/logistics"]
CMD ["serve"]
