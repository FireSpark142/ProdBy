package monitor

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"git-monitor-app/backup"    // Use correct module path
	"git-monitor-app/config"    // Use correct module path
	"git-monitor-app/gitutil"   // Use correct module path
	"git-monitor-app/validator" // Use correct module path

	"github.com/fsnotify/fsnotify"
)

var (
	lastKnownHash string
	repoPath      string
	debounceTimer *time.Timer
	debounceMu    sync.Mutex // Protect timer access
	processingMu  sync.Mutex // Prevent concurrent processing of commits
	appConfig     *config.Config
)

// Start initializes and runs the file system watcher.
func Start(cfg *config.Config) {
	appConfig = cfg // Store config for access in callbacks
	repoPath = cfg.RepoPath
	gitDir := filepath.Join(repoPath, ".git")
	if _, err := os.Stat(gitDir); os.IsNotExist(err) {
		log.Fatalf("Monitor Error: '.git' directory not found in %s", repoPath)
		return
	}

	var err error
	lastKnownHash, err = gitutil.GetCurrentCommitHash(repoPath)
	if err != nil {
		log.Printf("Monitor Warning: Could not get initial commit hash for %s: %v. Will process the first detected commit.", repoPath, err)
		lastKnownHash = "" // Start fresh
	}
	log.Printf("Monitor: Starting monitoring for repo: %s", repoPath)
	if lastKnownHash != "" {
		log.Printf("Monitor: Initial commit hash: %s", lastKnownHash)
	}

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		log.Fatal("Monitor Error: Failed to create watcher:", err)
	}
	defer watcher.Close() // Ensure watcher is closed on exit

	// --- Add paths to watcher ---
	// Watching specific files can be brittle if Git internals change.
	// Watching directories might generate more events but is often more robust.
	// Key directories/files involved in commits:
	pathsToWatch := []string{
		gitDir,                        // Watch base .git dir for changes to HEAD, index etc.
		filepath.Join(gitDir, "refs"), // Watch for ref changes (branches, tags)
		// filepath.Join(gitDir, "logs"), // Watch logs for refs like HEAD - might be noisy
	}

	watchErrors := 0
	for _, p := range pathsToWatch {
		if _, err := os.Stat(p); err == nil {
			// Watch directory recursively - fsnotify might need manual recursion depending on platform/usage
			log.Printf("Monitor: Adding watch on: %s", p)
			err = addRecursiveWatch(watcher, p) // Use helper for recursion
			if err != nil {
				log.Printf("Monitor Error: Failed to add watch on %s: %v", p, err)
				watchErrors++
			}
		} else {
			log.Printf("Monitor Warning: Path %s does not exist, skipping watch.", p)
		}
	}

	if watchErrors > 0 {
		log.Printf("Monitor Warning: %d errors occurred adding watches. Monitoring might be incomplete.", watchErrors)
		// Decide if this is fatal - for now, continue if some watches were added.
		// if len(watcher.WatchList()) == 0 { log.Fatal("Monitor Error: Failed to add any watches.")}
	}

	log.Println("Monitor: Watcher started. Waiting for Git activity...")

	// --- Event Loop ---
	for {
		select {
		case event, ok := <-watcher.Events:
			if !ok {
				log.Println("Monitor: Watcher events channel closed.")
				return // Channel closed
			}
			// Log event details for debugging if needed
			// log.Printf("Monitor Event: Op=%s, Name=%s", event.Op, event.Name)

			// Filter events - React mainly to writes/creates/renames
			// Note: Rename/Chmod might also indicate commit finished. Write is common.
			if event.Has(fsnotify.Write) || event.Has(fsnotify.Create) || event.Has(fsnotify.Rename) {
				// Debounce: Reset timer on relevant events
				debounceMu.Lock()
				if debounceTimer != nil {
					debounceTimer.Stop()
				}
				debounceDuration := time.Duration(appConfig.DebounceSecs) * time.Second
				debounceTimer = time.AfterFunc(debounceDuration, handleCommitCheck)
				debounceMu.Unlock()
			}

		case err, ok := <-watcher.Errors:
			if !ok {
				log.Println("Monitor: Watcher errors channel closed.")
				return // Channel closed
			}
			log.Println("Monitor Error: Watcher error:", err)
		}
	}
}

// addRecursiveWatch adds watches to a directory and all its subdirectories.
func addRecursiveWatch(watcher *fsnotify.Watcher, rootPath string) error {
	err := filepath.WalkDir(rootPath, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			// Report error but continue walking other paths if possible
			log.Printf("Monitor Warning: Error accessing path %q: %v", path, walkErr)
			return nil // Continue walking if possible, or return walkErr to stop
		}
		if d.IsDir() {
			// Avoid watching .git/objects as it's extremely noisy and usually not needed directly
			// Add other ignores if necessary (e.g., hooks)
			base := filepath.Base(path)
			if base == "objects" || base == "hooks" {
				// log.Printf("Monitor: Skipping watch on subdir: %s", path)
				return filepath.SkipDir // Don't descend into this directory
			}

			// log.Printf("Monitor: Adding recursive watch on dir: %s", path)
			err := watcher.Add(path)
			if err != nil {
				// Log error but continue trying to add other watches
				log.Printf("Monitor Error: Failed to add watch on directory %s: %v", path, err)
			}
		}
		return nil // Continue walking
	})

	if err != nil {
		return fmt.Errorf("error during recursive watch setup for %s: %w", rootPath, err)
	}
	// Also add watch to the root path itself
	if err := watcher.Add(rootPath); err != nil {
		log.Printf("Monitor Error: Failed to add watch on root path %s: %v", rootPath, err)
		return err
	}
	return nil
}

// handleCommitCheck is called after the debounce timer fires.
// It checks if a new commit has occurred and triggers validation/backup.
func handleCommitCheck() {
	// Ensure only one check runs at a time
	if !processingMu.TryLock() {
		log.Println("Monitor: Commit check already in progress, skipping.")
		return
	}
	defer processingMu.Unlock()

	log.Println("Monitor: Debounce triggered, checking for new commit...")
	currentHash, err := gitutil.GetCurrentCommitHash(repoPath)
	if err != nil {
		log.Printf("Monitor Error: Could not get current commit hash during check: %v", err)
		return
	}

	if currentHash != "" && currentHash != lastKnownHash {
		log.Printf("Monitor: New commit detected! Previous: %s, Current: %s", lastKnownHash, currentHash)
		commitHashToProcess := currentHash // Capture the hash we are processing

		// Update state *before* processing to prevent reprocessing if errors occur mid-way
		originalLastHash := lastKnownHash
		lastKnownHash = currentHash

		// Get changed files for the *new* commit
		changedFiles, err := gitutil.GetChangedFilesInCommit(repoPath, commitHashToProcess)
		if err != nil {
			log.Printf("Monitor Error: Failed getting changed files for commit %s: %v. Skipping processing.", commitHashToProcess, err)
			lastKnownHash = originalLastHash // Revert state if we couldn't get files
			return
		}

		// Validate the changes
		log.Printf("Monitor: Starting validation for commit %s...", commitHashToProcess)
		isValid, validationErrors := validator.Validate(repoPath, changedFiles)

		if isValid {
			log.Printf("Monitor: Commit %s PASSED validation.", commitHashToProcess)
			log.Printf("Monitor: Starting backup for commit %s...", commitHashToProcess)

			err := backup.RunBackup(repoPath, commitHashToProcess, &appConfig.Backup)
			if err != nil {
				log.Printf("Monitor Error: Backup FAILED for commit %s: %v", commitHashToProcess, err)
				// Should we revert lastKnownHash here? Maybe not, commit is valid but backup failed.
				// Maybe add retry logic? For now, just log.
			} else {
				log.Printf("Monitor: Backup SUCCEEDED for commit %s.", commitHashToProcess)
			}
		} else {
			log.Printf("Monitor: Commit %s FAILED validation:", commitHashToProcess)
			for _, verr := range validationErrors {
				log.Printf("  - %s", verr)
			}
			log.Printf("Monitor: Backup SKIPPED for invalid commit %s.", commitHashToProcess)
			// TODO: Optional - Send system notification
		}
	} else if currentHash == lastKnownHash {
		log.Println("Monitor: No new commit detected since last check.")
	} else {
		log.Printf("Monitor: Current commit hash is empty, skipping check (perhaps repo initializing?).")
	}
}
