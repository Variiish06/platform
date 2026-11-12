# Two-stage build: compile (and test) with the full Go toolchain, then
# copy only the finished binary into an empty image. The result is about
# 15 MB and contains no shell, package manager or other tools.

FROM golang:1.26-alpine AS build
WORKDIR /src
# Dependencies first: this layer is reused until go.mod/go.sum change.
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# A failing test fails the build, so a broken auth change cannot ship.
RUN go vet ./... && go test ./...
# CGO_ENABLED=0 produces a static binary that runs on "scratch".
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /portal ./cmd/portal

FROM scratch
COPY --from=build /portal /portal
ENTRYPOINT ["/portal"]
CMD ["serve"]
