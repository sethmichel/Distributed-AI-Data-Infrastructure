#!/bin/bash
echo "Tearing down containers and volumes..."
docker-compose down -v

echo "Rebuilding and starting containers..."
docker-compose up -d --build

echo "Done! Services are running."
docker-compose ps

