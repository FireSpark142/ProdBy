package validator

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"git-monitor-app/gitutil" // Use correct module path
)

// Precompile regexps for efficiency
var (
	projectExtRegex    = regexp.MustCompile(`\.(flp|rpp|song|project|cpr|ptx|logicx|als)$`)
	exportExtRegex     = regexp.MustCompile(`\.(mp3|wav|flac)$`)
	exportStatusRegex  = regexp.MustCompile(`^(progress|unmixed|rough|mixed|finalmix|roughmaster|mastered|finalmaster)$`)
	projectFolderRegex = regexp.MustCompile(`^([a-zA-Z0-9_.,-]+)-([a-zA-Z0-9_.,-]+(?:,[a-zA-Z0-9_.,-]+)*)-([a-zA-Z0-9_.,-]+)-([0-9]{2,3})bpm-prodby\.([a-zA-Z0-9_.,-]+)$`)
	allowedRootFiles   = map[string]bool{"README.md": true, ".gitignore": true, "config.toml": true}
	requiredRootFiles  = []string{"README.md", ".gitignore"}
)

// Validate checks the list of changed files against the predefined rules.
func Validate(repoPath string, changedFiles []string) (bool, []string) {
	var errors []string
	isValid := true

	addError := func(format string, args ...interface{}) {
		errors = append(errors, fmt.Sprintf(format, args...))
		isValid = false
	}

	if len(changedFiles) == 0 {
		fmt.Println("Validator: No changed files to validate.")
		// Still check required files even if no changes staged in this commit
	} else {
		fmt.Printf("Validator: Checking %d changed file(s)...\n", len(changedFiles))
		for _, file := range changedFiles {
			fmt.Printf("Validator: Checking file: %s\n", file)
			hasFileError := false // Track if *this specific file* has an error

			// --- General Rule 1: No spaces ---
			if strings.Contains(file, " ") {
				addError("Path contains spaces: '%s'", file)
				hasFileError = true
			}

			// Use filepath.ToSlash for consistent path separators
			filePath := filepath.ToSlash(file)
			dir := filepath.ToSlash(filepath.Dir(filePath))
			base := filepath.Base(filePath)

			// --- Structure & Naming Rules ---

			// Rule: Root files
			if dir == "." {
				if !allowedRootFiles[base] {
					addError("Unexpected file in root directory: '%s'", file)
					hasFileError = true
				}
			} else if strings.HasPrefix(filePath, "src/") {
				parts := strings.Split(filePath, "/")
				if parts[0] != "src" {
					addError("'src' directory component must be lowercase: '%s' in path '%s'", parts[0], file)
					hasFileError = true
				}

				// Rule: src/projects directory
				if len(parts) > 1 && parts[1] == "projects" {
					if len(parts) > 2 { // We have a project folder level or deeper
						projectFolder := parts[2]
						if !projectFolderRegex.MatchString(projectFolder) {
							addError("Invalid project folder name format: '%s' in path '%s'", projectFolder, file)
							hasFileError = true
						} else {
							// Path relative to project folder
							pathInsideProject := strings.Join(parts[3:], "/")

							if pathInsideProject == "" {
								// This case should ideally not happen for files from `git show --name-only`
								// but might if a directory itself was listed?
								fmt.Printf("Validator: Note - Project directory '%s' itself listed.\n", projectFolder)
							} else if !strings.Contains(pathInsideProject, "/") { // File directly in project folder
								baseName := parts[len(parts)-1]
								ext := filepath.Ext(baseName)
								baseNameNoExt := strings.TrimSuffix(baseName, ext)

								if baseNameNoExt != projectFolder {
									addError("Project filename base must match folder name. Expected '%s.*', found '%s' in path '%s'", projectFolder, baseName, file)
									hasFileError = true
								}
								if !projectExtRegex.MatchString(strings.ToLower(baseName)) { // Check extension case-insensitively maybe? Using ToLower here.
									addError("Invalid file extension for project file '%s'. Allowed: .flp, .rpp, .song, .project, .cpr, .ptx, .logicx, .als. Path: '%s'", baseName, file)
									hasFileError = true
								}
							} else if strings.HasPrefix(pathInsideProject, "exports/") { // Inside exports/
								if parts[3] != "exports" {
									addError("'exports' directory component must be lowercase: '%s' in path '%s'", parts[3], file)
									hasFileError = true
								}

								if len(parts) > 4 { // File inside exports/
									exportBaseName := parts[len(parts)-1]
									exportExt := filepath.Ext(exportBaseName)
									exportBaseNameNoExt := strings.TrimSuffix(exportBaseName, exportExt)

									// Extract base part and status (zzz)
									match := regexp.MustCompile(`^(.*)-([^-]+)$`).FindStringSubmatch(exportBaseNameNoExt)
									if len(match) != 3 {
										addError("Export filename '%s' does not match expected format '[project_base]-[status]'. Path: '%s'", exportBaseName, file)
										hasFileError = true
									} else {
										exportBase := match[1]
										exportStatus := match[2]

										if exportBase != projectFolder {
											addError("Export filename base must match project folder name. Expected '%s-[status].*', found '%s' in path '%s'", projectFolder, exportBaseName, file)
											hasFileError = true
										}
										if !exportStatusRegex.MatchString(exportStatus) {
											addError("Invalid status identifier '%s' in export filename '%s'. Allowed: progress, unmixed, rough, mixed, finalmix, roughmaster, mastered, finalmaster. Path: '%s'", exportStatus, exportBaseName, file)
											hasFileError = true
										}
									}
									// Check export file extension
									if !exportExtRegex.MatchString(strings.ToLower(exportBaseName)) { // Case-insensitive check
										addError("Invalid file extension for export file '%s'. Allowed: .mp3, .wav, .flac. Path: '%s'", exportBaseName, file)
										hasFileError = true
									}
								}
								// else: it's just the exports/ directory itself being added/modified - ignore file checks
							} else {
								// Rule: Other files/dirs inside project folder not allowed
								addError("Unexpected file or directory inside project folder: '%s' in path '%s'. Only project file and 'exports/' dir allowed directly under '%s/'", pathInsideProject, file, projectFolder)
								hasFileError = true
							}
						}
					} else {
						// File directly under src/projects/ - Not allowed
						addError("Files are not allowed directly inside 'src/projects/'. Place them in a named project folder: '%s'", file)
						hasFileError = true
					}
				} else if len(parts) > 1 && parts[1] != "projects" {
					// Rule: src/* (folders other than projects) - Not monitored inside
					fmt.Printf("Validator: Skipping detailed checks for non-project path inside src/: '%s'\n", file)
				}
				// else: file is src/something - already checked parts[0] == "src"

			} else if dir != "." { // Not root, not src/*
				addError("Unexpected top-level file or directory: '%s'. Only 'src/', 'README.md', 'config.toml', '.gitignore' allowed.", file)
				hasFileError = true
			}
			if hasFileError {
				fmt.Printf("Validator: Error found for file: %s\n", file)
			}

		} // End file loop
	} // End check if changedFiles not empty

	// --- Check for required files existence in the repository index ---
	// This check runs regardless of validation status of changed files
	fmt.Println("Validator: Checking existence of required files in repository...")
	reqFilesFound := true
	for _, reqFile := range requiredRootFiles {
		if !gitutil.CheckFileExists(repoPath, reqFile) {
			addError("Required file '%s' not found in repository index.", reqFile)
			reqFilesFound = false
		}
	}
	if reqFilesFound {
		fmt.Println("Validator: All required files found.")
	}

	return isValid, errors
}
