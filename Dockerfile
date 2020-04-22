FROM golang:1.14-alpine

RUN apk add --no-cache git wget
RUN go get github.com/jstemmer/go-junit-report

COPY . /go/src/github.com/cyverse-de/interapps-runner
ENV CGO_ENABLED=0
RUN wget https://github.com/upx/upx/releases/download/v3.95/upx-3.95-amd64_linux.tar.xz \
 && tar -xJvf upx-3.95-amd64_linux.tar.xz upx-3.95-amd64_linux/upx \
 && cd /go/src/github.com/cyverse-de/interapps-runner \
 && go install ./... \
 && cd - \
 && ./upx-3.95-amd64_linux/upx --ultra-brute /go/bin/interapps-runner \
 && rm -rf upx-3.95-amd64_linux*

ENTRYPOINT ["interapps-runner"]
CMD ["--help"]

ARG git_commit=unknown
ARG version="2.9.0"
ARG descriptive_version=unknown

LABEL org.cyverse.git-ref="$git_commit"
LABEL org.cyverse.version="$version"
LABEL org.cyverse.descriptive-version="$descriptive_version"
LABEL org.label-schema.vcs-ref="$git_commit"
LABEL org.label-schema.vcs-url="https://github.com/cyverse-de/interapps-runner"
LABEL org.label-schema.version="$descriptive_version"
