ARG WORKSPACE_PROBE_BUILDER=golang:1.25-alpine
ARG SANDBOX_BASE_IMAGE=python:3.13-slim
FROM ${WORKSPACE_PROBE_BUILDER} AS workspace-probe-builder

WORKDIR /src
COPY go.mod go.sum ./
COPY cmd/workspace-probe ./cmd/workspace-probe
COPY internal/fuseprotocol ./internal/fuseprotocol
COPY internal/workspaceprobe ./internal/workspaceprobe
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags='-s -w' \
    -o /out/workspace-probe ./cmd/workspace-probe

FROM ${SANDBOX_BASE_IMAGE}

# Use China mirror for Debian packages
RUN sed -i 's|http://deb.debian.org|https://mirrors.aliyun.com|g' /etc/apt/sources.list.d/debian.sources

# System tools
RUN apt-get update
RUN apt-get install -y --no-install-recommends \
    curl wget jq git openssh-client zip unzip iproute2
RUN apt-get install -y findutils gawk imagemagick ffmpeg \
    && rm -rf /var/lib/apt/lists/*

# Node.js 20
RUN curl -fsSL https://deb.nodesource.com/setup_20.x | bash - \
    && apt-get install -y --no-install-recommends nodejs \
    && rm -rf /var/lib/apt/lists/* \
    && npm config set registry https://registry.npmmirror.com

# Use Tsinghua pip mirror
RUN pip config set global.index-url https://pypi.tuna.tsinghua.edu.cn/simple

# Python packages
RUN pip install --no-cache-dir \
    numpy==2.2.0 \
    pandas==2.2.3 \
    matplotlib==3.9.3 \
    requests==2.32.3 \
    Pillow==11.0.0 \
    python-pptx==1.0.2 \
    openpyxl==3.1.5 \
    scipy==1.14.1 \
    scikit-learn==1.5.2 \
    sympy==1.13.3 \
    beautifulsoup4==4.12.3

# Python 文档处理库
RUN pip install --no-cache-dir \
    pymupdf \
    pdfplumber \
    reportlab \
    openpyxl \
    python-docx \
    playwright

# Playwright Chromium 及系统依赖
ENV PLAYWRIGHT_BROWSERS_PATH=/opt/playwright-browsers
RUN playwright install-deps chromium \
  && playwright install chromium \
  && chmod -R a+r /opt/playwright-browsers

# Jupyter Notebook
RUN pip install --no-cache-dir \
    jupyter \
    ipykernel \
    nbformat \
    nbconvert

# Python Common Expansion Pack
RUN pip install --no-cache-dir \
    xmltodict==1.0.4 \
    pyyaml==6.0.2 \
    httpx==0.28.1 \
    toml==0.10.2 \
    rich==13.9.4 \
    tqdm==4.67.1 \
    seaborn==0.13.2 \
    python-dateutil==2.9.0 \
    opencv-python-headless==4.10.0.84 \
    cryptography==44.0.0 \
    bcrypt==4.2.1 \
    aiohttp==3.11.10 \
    jieba==0.42.1 \
    pypinyin==0.53.0 \
    chardet==5.2.0 \
    pyarrow==18.1.0 \
    pytz==2024.2 \
    tabulate==0.9.0 \
    faker==33.1.0 \
    click==8.1.8 \
    orjson==3.10.12

# TypeScript + global Node packages
RUN npm install -g \
    typescript@5.6.3 \
    ts-node@10.9.2 \
    tsx@4.19.2

WORKDIR /opt/sandbox-libs
RUN echo '{"name":"sandbox-libs","private":true,"dependencies":{"axios":"^1.7.0","lodash":"^4.17.21","cheerio":"^1.0.0","csv-parse":"^5.6.0","exceljs":"^4.4.0","zod":"^3.23.0","dayjs":"^1.11.0","uuid":"^10.0.0","dotenv":"^16.0.0","marked":"^14.0.0","jsdom":"^25.0.0"}}' > package.json && \
    npm install --production
ENV NODE_PATH=/opt/sandbox-libs/node_modules

# Create non-root user
RUN groupadd -r sandbox && useradd -r -g sandbox -u 1000 -m sandbox
RUN chown -R sandbox:sandbox /opt/sandbox-libs

# Create workspace
RUN mkdir -p /workspace && chown sandbox:sandbox /workspace

# Non-privileged fixed-path workspace health/quiesce probe. The ordinary image
# intentionally does not contain s3fs or the root workspace supervisor.
COPY --from=workspace-probe-builder /out/workspace-probe /usr/local/bin/workspace-probe
RUN chown root:root /usr/local/bin/workspace-probe \
    && chmod 0755 /usr/local/bin/workspace-probe

WORKDIR /workspace
USER sandbox

CMD ["sleep", "infinity"]
