package main

import (
	"fmt"
	"os"
	"os/exec"
)

func main() {
	fmt.Println("Uninstalling")
	cmdDelete := exec.Command(
		"rosa", "delete", "cluster",
		"--region", os.Getenv("AWS_REGION"),
		"-c", os.Getenv("CLUSTER_NAME"),
		"-y",
	)
	err := runCmd1(cmdDelete)
	if err != nil {
		fmt.Println(err)
	}
	fmt.Println("Uninstalled!")
}

func runCmd1(cmd *exec.Cmd) error {
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	err := cmd.Start()
	if err != nil {
		return err
	}

	err = cmd.Wait()
	if err != nil {
		return err
	}
	return nil
}
