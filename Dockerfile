FROM --platform=${BUILDPLATFORM:-linux/amd64} golang:1.18-alpine AS build
ARG TARGETPLATFORM
ARG BUILDPLATFORM
ARG VERSION
RUN echo "running on $BUILDPLATFORM, building for $TARGETPLATFORM"
RUN apk add --no-cache git make bash build-base libstdc++
WORKDIR /app
COPY . .
RUN make

FROM --platform=${TARGETPLATFORM:-linux/amd64} alpine:latest as prod
RUN addgroup -S app \
    && adduser -S -G app app \
    && apk --no-cache add \
    ca-certificates libstdc++
USER app
COPY --from=build  /app/bin/eth.store /bin/eth.store
ENTRYPOINT ["/bin/eth.store"]
