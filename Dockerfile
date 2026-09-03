# A container image from day one.
#
# D19 identifies exactly two things v1 owes its future Kubernetes life, and this
# is one of them: three lines for a static Go binary now, an annoying retrofit
# into a release process later. Building it from the first commit also keeps it
# honest -- an image that is built every day cannot quietly stop working.

FROM golang:1.26-alpine AS build
WORKDIR /src

# Dependencies first, so the module cache survives source edits.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
ARG VERSION=dev
# CGO_ENABLED=0 is what makes the binary static, which is only possible because
# D4 chose modernc.org/sqlite (pure Go) over the cgo bindings.
#
# What that buys, in order of how much it matters: a worker can run natively on
# a Mac, which D20 requires because a job that drives Shortcuts cannot run in a
# container; the same artifact cross-compiles for every platform a release
# ships; and Almanac can embed it (D18).
#
# It also makes the base below possible, which is the least important of the
# four -- see the note there.
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /je ./cmd/je

# An empty base, for now. This is a convenience and not a principle (D23): it is
# small and has no OS packages to patch, and it costs about 18 MB against ~47 MB
# for a base carrying git. When the write path of D23 lands, the control plane
# needs a real `git` and this becomes a base that has one -- that trade is
# already decided, and 29 MB was never a reason to argue about architecture.
#
# What is NOT riding on this: the guarantee that the control plane runs none of
# your code. That comes from C11 -- every run goes to a worker -- not from the
# filesystem being empty. The empty image illustrates the rule; it does not
# enforce it.
FROM scratch
# Certificates, so jobs and the engine can reach HTTPS endpoints. Timezone data
# is compiled into the binary itself (see cmd/je/main.go), so no zoneinfo copy
# is needed -- but TZ must still be set explicitly, because a container defaults
# to UTC and D9's schedules mean local time to a human.
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=build /je /je

ENV JE_DATA_DIR=/var/lib/je
ENV TZ=UTC
VOLUME /var/lib/je
EXPOSE 7620

# One image, two components (D20). `control-plane run` and `worker run` are the
# two halves; compose.yaml runs both from this image, and so does a cluster.
#
# Shipping them together is C10 made cheap: there is one artifact and one
# version, so the skew the control plane refuses to tolerate cannot arise by
# accident in a deployment that pulls one tag.
#
# Note what a worker image usually is NOT: this one can run nothing but /je
# itself, which is correct for the system worker (whose jobs are the engine's
# own) and wrong for yours. A worker that runs Python jobs needs a Python image
# with /je copied in -- that is the job author's concern, and it is only
# expressible at all because the control plane never runs anybody's code.
#
# A worker that must reach the machine itself does not use an image at all: it
# is `je worker run` on that Mac, which is the case the static binary exists
# for (D20).
#
# 0.0.0.0 inside the container: the trust boundary is the network, not the
# loopback interface (D19's rider on N1). Nothing here weakens the local
# default, which stays 127.0.0.1.
ENTRYPOINT ["/je"]
CMD ["control-plane", "run", "--addr", "0.0.0.0:7620"]
