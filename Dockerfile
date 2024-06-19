FROM --platform=${BUILDPLATFORM:-linux/amd64} golang:1.21-alpine@sha256:c0ea884eb6fbeff67789797cb565d87995a125d0adc0907e46c6566498fd3ce2 AS build
ARG TARGETPLATFORM
ARG BUILDPLATFORM
ARG VERSION
RUN echo "running on $BUILDPLATFORM, building for $TARGETPLATFORM"
RUN apk add --no-cache git make bash build-base libstdc++
WORKDIR /app
COPY . .
RUN make

FROM --platform=${TARGETPLATFORM:-linux/amd64} alpine:latest@sha256:77726ef6b57ddf65bb551896826ec38bc3e53f75cdde31354fbffb4f25238ebd as prod
RUN addgroup -S app \
    && adduser -S -G app app \
    && apk --no-cache add \
    ca-certificates libstdc++
USER app
COPY --from=build  /app/bin/eth.store /bin/eth.store
ENTRYPOINT ["/bin/eth.store"]
