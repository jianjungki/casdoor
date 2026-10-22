ARG NODE_IMAGE=node:20.20.1
ARG GO_IMAGE=golang:1.25.8
ARG ALPINE_IMAGE=alpine:latest
ARG DEBIAN_IMAGE=debian:latest

FROM ${NODE_IMAGE} AS FRONT
WORKDIR /web

# Copy only dependency files first for better caching
COPY ./web/package.json ./web/yarn.lock ./
RUN yarn install --frozen-lockfile --network-timeout 1000000

# Copy source files and build
COPY ./web .
RUN NODE_OPTIONS="--max-old-space-size=4096" yarn run build

FROM ${GO_IMAGE} AS BACK
# GOPROXY to avoid timeouts when fetching Go packages during the build.
# It can be overridden at build time with:
#   docker build --build-arg GOPROXY=https://proxy.golang.org,direct ...
ARG GOPROXY=https://goproxy.cn,direct
ENV GOPROXY=$GOPROXY
WORKDIR /go/src/casdoor

# Copy only go.mod and go.sum first for dependency caching
COPY go.mod go.sum ./
RUN go mod download

# Copy source files
COPY . .

RUN go test -v -run TestGetVersionInfo ./util/system_test.go ./util/system.go ./util/variable.go
RUN ./build.sh

FROM ${ALPINE_IMAGE} AS STANDARD
LABEL MAINTAINER="https://casdoor.org/"
ARG USER=casdoor
ARG TARGETOS
ARG TARGETARCH
ENV BUILDX_ARCH="${TARGETOS:-linux}_${TARGETARCH:-amd64}"

# Install all system packages in a single layer. Like build.sh's Google check,
# probe the default Alpine CDN first; when it is unreachable (blocked or flaky
# in restricted networks, causing "Socket not connected" / apk exit code 6),
# fall back to the Aliyun mirror. apk is retried on transient network errors,
# and after 3 failures over HTTPS the mirror is downgraded to HTTP (which was
# previously done unconditionally via "sed -i 's/https/http/'").
RUN set -eux; \
    if ! timeout 10 wget -q -O /dev/null https://dl-cdn.alpinelinux.org/alpine/; then \
        echo "Alpine CDN is unreachable, apk mirror is switched to Aliyun: https://mirrors.aliyun.com"; \
        sed -i 's#dl-cdn.alpinelinux.org#mirrors.aliyun.com#g' /etc/apk/repositories; \
    fi; \
    for i in 1 2 3 4 5 6; do \
        if apk add --no-cache sudo tzdata curl ca-certificates && update-ca-certificates; then \
            break; \
        fi; \
        if [ "$i" = 3 ]; then \
            if grep -q 'dl-cdn.alpinelinux.org' /etc/apk/repositories; then \
                echo "apk keeps failing with the Alpine CDN, switching to Aliyun: https://mirrors.aliyun.com"; \
                sed -i 's#dl-cdn.alpinelinux.org#mirrors.aliyun.com#g' /etc/apk/repositories; \
            elif grep -q 'https://' /etc/apk/repositories; then \
                echo "apk over HTTPS keeps failing, downgrading to HTTP"; \
                sed -i 's#https://#http://#g' /etc/apk/repositories; \
            fi; \
        fi; \
        if [ "$i" -ge 6 ]; then echo "apk add failed after 6 attempts" >&2; exit 1; fi; \
        echo "apk add attempt $i failed, retrying in 5 seconds..."; \
        sleep 5; \
    done

RUN adduser -D $USER -u 1000 \
    && echo "$USER ALL=(ALL) NOPASSWD: ALL" > /etc/sudoers.d/$USER \
    && chmod 0440 /etc/sudoers.d/$USER \
    && mkdir logs \
    && chown -R $USER:$USER logs

USER 1000
WORKDIR /
COPY --from=BACK --chown=$USER:$USER /go/src/casdoor/server_${BUILDX_ARCH} ./server
COPY --from=BACK --chown=$USER:$USER /go/src/casdoor/swagger ./swagger
COPY --from=BACK --chown=$USER:$USER /go/src/casdoor/conf/app.conf ./conf/app.conf
COPY --from=FRONT --chown=$USER:$USER /web/build ./web/build

ENTRYPOINT ["/server"]


FROM ${DEBIAN_IMAGE} AS ALLINONE
LABEL MAINTAINER="https://casdoor.org/"
ARG TARGETOS
ARG TARGETARCH
ENV BUILDX_ARCH="${TARGETOS:-linux}_${TARGETARCH:-amd64}"

RUN apt update
RUN apt install -y ca-certificates lsof && update-ca-certificates

WORKDIR /
COPY --from=BACK /go/src/casdoor/server_${BUILDX_ARCH} ./server
COPY --from=BACK /go/src/casdoor/swagger ./swagger
COPY --from=BACK /go/src/casdoor/docker-entrypoint.sh /docker-entrypoint.sh
COPY --from=BACK /go/src/casdoor/conf/app.conf ./conf/app.conf
COPY --from=FRONT /web/build ./web/build

ENTRYPOINT ["/bin/bash"]
CMD ["/docker-entrypoint.sh"]
