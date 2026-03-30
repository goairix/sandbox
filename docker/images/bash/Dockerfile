FROM alpine:3.20

# Install common CLI tools
RUN apk add --no-cache \
    bash \
    curl \
    wget \
    jq \
    yq \
    git \
    openssh-client \
    zip \
    unzip \
    tar \
    gzip \
    findutils \
    coreutils \
    grep \
    sed \
    awk \
    imagemagick \
    ffmpeg \
    python3 \
    py3-pip

# Create non-root user
RUN addgroup -S sandbox && adduser -S -G sandbox -u 1000 sandbox

# Create workspace
RUN mkdir -p /workspace && chown sandbox:sandbox /workspace

WORKDIR /workspace
USER sandbox

CMD ["sleep", "infinity"]
