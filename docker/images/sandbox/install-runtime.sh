#!/bin/sh
set -eu

export DEBIAN_FRONTEND=noninteractive

sed -i 's|http://deb.debian.org|https://mirrors.aliyun.com|g' \
  /etc/apt/sources.list.d/debian.sources

apt-get update
apt-get install -y --no-install-recommends \
  curl wget jq git openssh-client zip unzip iproute2 \
  findutils gawk imagemagick ffmpeg
rm -rf /var/lib/apt/lists/*

curl -fsSL https://deb.nodesource.com/setup_20.x | bash
apt-get install -y --no-install-recommends nodejs
rm -rf /var/lib/apt/lists/*
npm config set registry https://registry.npmmirror.com

pip config set global.index-url https://pypi.tuna.tsinghua.edu.cn/simple
pip install --no-cache-dir \
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

pip install --no-cache-dir \
  pymupdf \
  pdfplumber \
  reportlab \
  openpyxl \
  python-docx \
  playwright

export PLAYWRIGHT_BROWSERS_PATH=/opt/playwright-browsers
playwright install-deps chromium
playwright install chromium
chmod -R a+r /opt/playwright-browsers

pip install --no-cache-dir \
  jupyter \
  ipykernel \
  nbformat \
  nbconvert

pip install --no-cache-dir \
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

npm install -g \
  typescript@5.6.3 \
  ts-node@10.9.2 \
  tsx@4.19.2

install -d -m 0755 /opt/sandbox-libs
cd /opt/sandbox-libs
printf '%s\n' \
  '{"name":"sandbox-libs","private":true,"dependencies":{"axios":"^1.7.0","lodash":"^4.17.21","cheerio":"^1.0.0","csv-parse":"^5.6.0","exceljs":"^4.4.0","zod":"^3.23.0","dayjs":"^1.11.0","uuid":"^10.0.0","dotenv":"^16.0.0","marked":"^14.0.0","jsdom":"^25.0.0"}}' \
  > package.json
npm install --production

groupadd -g 1000 sandbox
useradd -g sandbox -u 1000 -m sandbox
chown -R sandbox:sandbox /opt/sandbox-libs
install -d -o sandbox -g sandbox -m 0755 /workspace
