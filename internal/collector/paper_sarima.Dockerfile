FROM python:3.11-slim
WORKDIR /app
COPY paper_sarima_requirements.txt /app/requirements.txt
RUN pip install --no-cache-dir -r /app/requirements.txt
COPY paper_sarima_feed.py /app/paper_sarima_feed.py
EXPOSE 8443
ENTRYPOINT ["python", "/app/paper_sarima_feed.py"]