**Startup**
- (assumes WSL2) open powershell
    - wsl -d Ubuntu
- run this to see if the k3 cluster is ready
    - sudo k3s kubectl get nodes
- k3 should have a local registry (we're not using google/aws/microsoft tools to autmate this): 
- start docker
    - sudo systemctl start docker
    - sudo systemctl enable docker


**Docker & Kubernetes (k3)**
- Summary: k3 can't build code into images, so we're using a lightweight version of docker inside ubuntu for that. all other docker uses are removed.

- old version used only docker. we're building this for 50 nodes, so we're replacing it with kubernetnes (k3) for that
- docker is fully removed except k3 can't build code into an image so we need docker for that. we're not using docker to run containers. 
- docker compose is removed, and we're instead using docker engine inside WSL2. this uses the command line to build images instead of a heavy docker app
- new process
    - code:  write code
    - build: docker build whatever (docker build -t my-service:v1 .)
    - ship:  docker push whatever (k3s ctr images import my-service.tar OR save to tar and import.)
    - run:   dubectl apply -f k8s/  # this might be wrong

images
- Docker stores images in /var/lib/docker (managed by the Docker daemon)
- K3s stores images in /var/lib/rancher/k3s/agent/containerd (managed by containerd)
- image process
    - build the image
        - docker build -t my-app:v1 .
    - import image to k3
        - docker save my-app:v1 | sudo k3s ctr images import -
    - now k3 can use the image
    - NOTE: My deployment YAML needs imagePullPolicy: Never or IfNotPresent so k3 doesn't try to pull it from Docker Hub