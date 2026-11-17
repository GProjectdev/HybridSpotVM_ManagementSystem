FROM python:3.11-slim
WORKDIR /app
COPY paper_sarima_replay.py /app/paper_sarima_replay.py
USER 65532:65532
EXPOSE 8443
ENTRYPOINT ["python", "/app/paper_sarima_replay.py"]
