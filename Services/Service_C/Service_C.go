package service_c

import (
	"log"
	"os"
	"os/exec"
	"path/filepath"
)

/*
this file only exists to keep connections alive and grpc active. service c is called infrequently so for now we're
we're using this to keep it up
*/

// StartServiceC launches the Python drift detection worker.
// It blocks until the process exits (which should be never, unless crashed/killed).
func StartServiceC() {
	var pythonPath string // for running python3 in k3 vs my local python for local developement
	if os.Getenv("RUN_IN_K3S") == "true" {
		pythonPath = "python3"
	} else {
		pythonPath = filepath.Join("venv", "Scripts", "python.exe")
	}

	scriptPath := filepath.Join("Services", "Service_C", "drift_detection.py")

	log.Printf("Service C: Launching Python Worker: %s %s", pythonPath, scriptPath)

	cmd := exec.Command(pythonPath, scriptPath)

	// Direct output to stdout/stderr for debugging
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		log.Printf("Service C: Critical Error - Failed to start Python Worker: %v", err)
		return
	}

	log.Printf("Service C: Python Worker started (PID: %d)", cmd.Process.Pid)

	// Wait for the process to exit
	if err := cmd.Wait(); err != nil {
		log.Printf("Service C: Python Worker exited with error: %v", err)
	} else {
		log.Println("Service C: Python Worker exited gracefully.")
	}
}
