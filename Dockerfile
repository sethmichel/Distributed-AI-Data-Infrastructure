# Start with a base that has both Go and Python capabilities
FROM golang:latest

# Install Python 3 and pip
RUN apt-get update && apt-get install -y python3 python3-pip python3-venv

# Set working directory
WORKDIR /app

# Copy Go dependencies and build
COPY go.mod go.sum ./
RUN go mod download

# Copy Python requirements and install
COPY Requirements.txt .
# Install python libs globally in the container to avoid venv complexity
# Using --break-system-packages because we are in a container environment where this is safe/intended
RUN pip3 install -r Requirements.txt --break-system-packages

# Copy the rest of the source code
COPY . .

# Build the Go binary
RUN go build -o ai-infra-app Main.go

# Expose ports (gRPC, Metrics, etc)
EXPOSE 8080 50052

# Run the binary
CMD ["./ai-infra-app"]
