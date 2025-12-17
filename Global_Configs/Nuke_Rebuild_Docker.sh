#!/bin/bash
echo "Tearing down containers and volumes..."
docker compose -f Docker_Compose.yml down -v

echo "Rebuilding and starting containers..."
docker compose -f Docker_Compose.yml up -d --build

echo "Done! Services are running."
docker compose -f Docker_Compose.yml ps