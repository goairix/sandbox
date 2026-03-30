FROM python:3.12-slim

# Create non-root user
RUN groupadd -r sandbox && useradd -r -g sandbox -u 1000 -m sandbox

# Install common Python libraries
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

# Create workspace directory
RUN mkdir -p /workspace && chown sandbox:sandbox /workspace

WORKDIR /workspace
USER sandbox

CMD ["sleep", "infinity"]
