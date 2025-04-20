package gitutil

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// GetCurrentCommitHash reads the commit hash pointed to by HEAD.
// Handles direct hash, symbolic refs, and falls back to `git rev-parse`.
func GetCurrentCommitHash(repoPath string) (string, error) {
	gitDir := filepath.Join(repoPath, ".git")
	headFile := filepath.Join(gitDir, "HEAD")

	contentBytes, err := os.ReadFile(headFile)
	if err != nil {
		// If HEAD file doesn't exist, maybe try rev-parse as a last resort?
		// Or just return error. Let's try rev-parse.
		return runGitRevParse(repoPath, "HEAD")
	}
	content := strings.TrimSpace(string(contentBytes))

	if strings.HasPrefix(content, "ref: ") {
		refPath := strings.TrimPrefix(content, "ref: ")
		// Try reading ref file directly
		refFile := filepath.Join(gitDir, refPath)
		hashBytes, err := os.ReadFile(refFile)
		if err == nil {
			return strings.TrimSpace(string(hashBytes)), nil
		}
		// If direct read fails (e.g., packed ref), fall back to rev-parse with the ref name
		return runGitRevParse(repoPath, refPath)

	} else if len(content) == 40 { // Check if it looks like a commit hash (detached HEAD)
		// Could add more robust hex check if needed
		return content, nil
	}

	// Fallback for unexpected HEAD content format
	return runGitRevParse(repoPath, "HEAD")
}

// runGitRevParse executes `git rev-parse` for a given revision.
func runGitRevParse(repoPath string, revision string) (string, error) {
	cmd := exec.Command("git", "-C", repoPath, "rev-parse", revision)
	out, err := cmd.Output()
	if err != nil {
		// Check if it's just that the ref doesn't exist yet (e.g., initial commit)
		// Use '_' to explicitly ignore the exitErr variable if not used
		if _, ok := err.(*exec.ExitError); ok {
			// stderrContent := string(exitErr.Stderr) // If you needed stderr, use exitErr here
			// Check stderr or exit code if needed to differentiate errors
			return "", fmt.Errorf("git rev-parse %s failed: %w", revision, err)
		}
		return "", fmt.Errorf("failed to execute git rev-parse: %w", err)

	}
	return strings.TrimSpace(string(out)), nil
}

// GetChangedFilesInCommit gets list of files changed in specific commit.
func GetChangedFilesInCommit(repoPath, commitHash string) ([]string, error) {
	// Using git show is often simpler than diff-tree for a single commit
	// Use -C repoPath to ensure git command runs in the correct directory
	cmd := exec.Command("git", "-C", repoPath, "show", "--pretty=", "--name-status", commitHash)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git show failed for commit %s: %w", commitHash, err)
	}

	files := []string{}
	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line != "" {
			// line format is typically "STATUS\tfilename" e.g., "M\tREADME.md"
			parts := strings.SplitN(line, "\t", 2)
			if len(parts) == 2 {
				// We only need the filename for validation based on the current rules
				// Status (parts[0]) could be useful for more advanced checks (e.g., ignore deleted 'D')
				status := parts[0]
				fileName := parts[1]
				// Handle renamed files format "RXXX\t oldpath\t newpath"
				if strings.HasPrefix(status, "R") {
					renameParts := strings.SplitN(line, "\t", 3)
					if len(renameParts) == 3 {
						files = append(files, renameParts[2]) // Validate the new path
					}
				} else if strings.HasPrefix(status, "C") { // Handle copied files format "CXXX\t oldpath\t newpath"
					copyParts := strings.SplitN(line, "\t", 3)
					if len(copyParts) == 3 {
						files = append(files, copyParts[2]) // Validate the new path
					}
				} else if status != "D" { // Exclude deleted files from validation
					files = append(files, fileName)
				}
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("error scanning git show output: %w", err)
	}
	return files, nil
}

// CheckFileExists checks if a file exists and is tracked by Git.
func CheckFileExists(repoPath, filePath string) bool {
	// git ls-files checks the index
	cmd := exec.Command("git", "-C", repoPath, "ls-files", "--error-unmatch", filePath)
	err := cmd.Run() // We only care about the exit code (0 if found, non-zero if not)
	return err == nil
}
