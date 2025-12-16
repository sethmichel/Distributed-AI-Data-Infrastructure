@echo off
echo Stopping System Services (Docker & Kafka)...

echo Stopping Docker services...
docker compose -f Global_Configs/docker-compose.yml down

echo Stopping Kafka...
if exist "C:\kafka_2.13-4.1.0\bin\windows\kafka-server-stop.bat" (
    call C:\kafka_2.13-4.1.0\bin\windows\kafka-server-stop.bat
) else (
    echo Kafka stop script not found at C:\kafka_2.13-4.1.0\bin\windows\kafka-server-stop.bat. Please check App.yaml and adjust this script.
)

echo Done.
pause

