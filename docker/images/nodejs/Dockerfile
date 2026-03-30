FROM node:20-slim

# Create non-root user (node:20-slim already has uid 1000 as 'node', remove it first)
RUN userdel -r node 2>/dev/null; \
    groupadd -r sandbox && useradd -r -g sandbox -u 1000 -m sandbox

# Install TypeScript and common packages globally
RUN npm install -g \
    typescript@5.6.3 \
    ts-node@10.9.2 \
    tsx@4.19.2

# Pre-install common packages in a global node_modules
WORKDIR /opt/sandbox-libs
RUN echo '{"name":"sandbox-libs","private":true,"dependencies":{"axios":"^1.7.0","lodash":"^4.17.21","cheerio":"^1.0.0","csv-parse":"^5.6.0","exceljs":"^4.4.0","zod":"^3.23.0"}}' > package.json && \
    npm install --production && chown -R sandbox:sandbox /opt/sandbox-libs
ENV NODE_PATH=/opt/sandbox-libs/node_modules

# Create workspace
RUN mkdir -p /workspace && chown sandbox:sandbox /workspace

WORKDIR /workspace
USER sandbox

CMD ["sleep", "infinity"]
