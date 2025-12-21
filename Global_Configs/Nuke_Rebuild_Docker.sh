#!/bin/bash
echo "Tearing down containers and volumes..."
docker compose -f docker_compose.yml down -v

echo "Rebuilding and starting containers..."
docker compose -f docker_compose.yml up -d --build

echo "Done! Services are running."
docker compose -f docker_compose.yml ps