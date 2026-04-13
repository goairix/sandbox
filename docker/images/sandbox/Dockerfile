FROM python:3.13-slim

# System tools
RUN apt-get update
RUN apt-get install -y --no-install-recommends \
    curl wget jq git openssh-client zip unzip
RUN apt-get install -y findutils gawk imagemagick ffmpeg \
    && rm -rf /var/lib/apt/lists/*

# Node.js 20
RUN curl -fsSL https://deb.nodesource.com/setup_20.x | bash - \
    && apt-get install -y --no-install-recommends nodejs \
    && rm -rf /var/lib/apt/lists/*

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
    reportlab \
    openpyxl \
    python-docx

# TypeScript + global Node packages
RUN npm install -g \
    typescript@5.6.3 \
    ts-node@10.9.2 \
    tsx@4.19.2

WORKDIR /opt/sandbox-libs
RUN echo '{"name":"sandbox-libs","private":true,"dependencies":{"axios":"^1.7.0","lodash":"^4.17.21","cheerio":"^1.0.0","csv-parse":"^5.6.0","exceljs":"^4.4.0","zod":"^3.23.0"}}' > package.json && \
    npm install --production
ENV NODE_PATH=/opt/sandbox-libs/node_modules

# Create non-root user
RUN groupadd -r sandbox && useradd -r -g sandbox -u 1000 -m sandbox
RUN chown -R sandbox:sandbox /opt/sandbox-libs

# Create workspace
RUN mkdir -p /workspace && chown sandbox:sandbox /workspace

WORKDIR /workspace
USER sandbox

CMD ["sleep", "infinity"]
