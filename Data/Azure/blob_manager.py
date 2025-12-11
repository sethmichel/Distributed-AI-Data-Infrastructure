import os
from azure.storage.blob import BlobServiceClient
from dotenv import load_dotenv

# Load environment variables from .env file
load_dotenv()

class BlobManager:
    def __init__(self, connection_string=None, container_name=None):
        self.connection_string = connection_string or os.getenv("AZURE_STORAGE_CONNECTION_STRING")
        self.container_name = container_name or os.getenv("AZURE_CONTAINER_NAME")
        
        if not self.connection_string:
            raise ValueError("AZURE_STORAGE_CONNECTION_STRING is not set. Please check your .env file.")
        if not self.container_name:
            raise ValueError("AZURE_CONTAINER_NAME is not set. Please check your .env file.")
            
        self.blob_service_client = BlobServiceClient.from_connection_string(self.connection_string)
        self.container_client = self.blob_service_client.get_container_client(self.container_name)
        
        # Create container if it doesn't exist
        try:
            if not self.container_client.exists():
                self.container_client.create_container()
                print(f"Created container: {self.container_name}")
        except Exception as e:
            print(f"Error checking/creating container: {e}")
            raise

    def upload_file(self, local_file_path, blob_name=None):
        """Uploads a local file to Azure Blob Storage."""
        if blob_name is None:
            blob_name = os.path.basename(local_file_path)
            
        blob_client = self.container_client.get_blob_client(blob_name)
        
        try:
            with open(local_file_path, "rb") as data:
                blob_client.upload_blob(data, overwrite=True)
            print(f"Successfully uploaded {local_file_path} to {blob_name}")
        except Exception as e:
            print(f"Failed to upload {local_file_path}: {e}")
            raise

    def download_file(self, blob_name, local_file_path):
        """Downloads a blob to a local file."""
        blob_client = self.container_client.get_blob_client(blob_name)
        
        try:
            os.makedirs(os.path.dirname(local_file_path), exist_ok=True)
            with open(local_file_path, "wb") as download_file:
                download_file.write(blob_client.download_blob().readall())
            print(f"Successfully downloaded {blob_name} to {local_file_path}")
        except Exception as e:
            print(f"Failed to download {blob_name}: {e}")
            raise

    def list_blobs(self):
        """Lists all blobs in the container."""
        try:
            blobs_list = self.container_client.list_blobs()
            print(f"Blobs in container '{self.container_name}':")
            for blob in blobs_list:
                print(f"\t{blob.name}")
            return blobs_list
        except Exception as e:
            print(f"Error listing blobs: {e}")
            raise

if __name__ == "__main__":
    # Example usage/test
    print("Initializing BlobManager...")
    try:
        manager = BlobManager()
        manager.list_blobs()
    except Exception as e:
        print(f"Initialization failed: {e}")

