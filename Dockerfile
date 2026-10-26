FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
ARG TARGETOS=linux
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags='-s -w' -o /chatgpt-oauth ./cmd/chatgpt-oauth
RUN mkdir /state && chown 65532:65532 /state

FROM gcr.io/distroless/static-debian13:nonroot
LABEL org.opencontainers.image.source="https://github.com/trapped/chatgpt-oauth"
LABEL org.opencontainers.image.licenses="MIT"
COPY --from=build /chatgpt-oauth /chatgpt-oauth
COPY --from=build --chown=65532:65532 /state /data
EXPOSE 10531
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s CMD ["/chatgpt-oauth", "healthcheck"]
ENTRYPOINT ["/chatgpt-oauth"]
CMD ["serve", "--listen", "0.0.0.0:10531"]
