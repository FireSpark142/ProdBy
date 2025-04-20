package backup

import (
	"compress/gzip"
	"context"
	"fmt"
	"log"
	"os/exec"
	"strings"

	"git-monitor-app/config" // Use correct module path

	"github.com/aws/aws-sdk-go-v2/aws"
	awsConfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// RunBackup performs the backup of a specific commit to S3/Wasabi.
func RunBackup(repoPath, commitHash string, cfg *config.BackupConfig) error {
	log.Printf("Backup: Starting backup process for commit %s", commitHash)

	// --- Configure AWS SDK ---
	log.Println("Backup: Configuring S3 client...")
	sdkConfigOptions := []func(*awsConfig.LoadOptions) error{}

	// 1. Custom Endpoint Resolver (Essential for Wasabi/S3 Compatible)
	if cfg.EndpointURL != "" {
		// Use EndpointResolverWithOptionsFunc for more control if needed, or simple StaticResolver
		// Ensure SigningRegion is set correctly if required by the provider
		customResolver := aws.EndpointResolverFunc(
			func(service, region string) (aws.Endpoint, error) {
                // Check if the service is S3, otherwise fallback
				if service == s3.ServiceID {
					return aws.Endpoint{
						URL:           cfg.EndpointURL,
						SigningRegion: cfg.Region, // Use region from config
						// Source: aws.EndpointSourceCustom, // Optional
					}, nil
				}
				// Fallback to default AWS resolution for other services (if any)
				return aws.Endpoint{}, &aws.EndpointNotFoundError{}
			})
		sdkConfigOptions = append(sdkConfigOptions, awsConfig.WithEndpointResolver(customResolver))
	} else {
        // If no endpoint URL, likely standard AWS, rely on default resolver + region
        if cfg.Region != "" {
             sdkConfigOptions = append(sdkConfigOptions, awsConfig.WithRegion(cfg.Region))
        }
    }


	// 2. Credentials
	// Prioritize credentials from config file ONLY if explicitly provided
	if cfg.AccessKeyID != "" && cfg.SecretKey != "" {
		log.Println("Backup: Using static credentials from config file.")
		staticCreds := credentials.NewStaticCredentialsProvider(cfg.AccessKeyID, cfg.SecretKey, "")
		sdkConfigOptions = append(sdkConfigOptions, awsConfig.WithCredentialsProvider(staticCreds))
	} else {
		log.Println("Backup: Using default AWS credential chain (Env Vars, Shared Config/Credentials).")
		// Default behavior of LoadDefaultConfig is to use the chain, so no specific option needed here
        // unless we need to force a specific profile, etc.
	}


	// Load the final configuration
	sdkConfig, err := awsConfig.LoadDefaultConfig(context.TODO(), sdkConfigOptions...)
	if err != nil {
		return fmt.Errorf("failed to load AWS SDK config: %w", err)
	}
    // Re-apply region if not implicitly set by resolver/defaults but present in config
    // (LoadDefaultConfig might overwrite simple WithRegion if resolver is complex)
    if cfg.Region != "" && sdkConfig.Region != cfg.Region{
        sdkConfig.Region = cfg.Region
        log.Printf("Backup: Explicitly setting region in loaded SDK config: %s", cfg.Region)
    }


	// Create S3 client
	s3Client := s3.NewFromConfig(sdkConfig)

	// --- Construct S3 Key ---
	backupFilename := fmt.Sprintf("commit-%s.tar.gz", commitHash)
	s3Key := backupFilename
	if cfg.Prefix != "" {
		// Ensure prefix doesn't start/end with slash for clean joining
		cleanPrefix := strings.Trim(cfg.Prefix, "/")
		if cleanPrefix != "" {
			s3Key = fmt.Sprintf("%s/%s", cleanPrefix, backupFilename)
		}
	}
	s3Path := fmt.Sprintf("s3://%s/%s", cfg.Bucket, s3Key)
	log.Printf("Backup: Target S3 Key: %s\n", s3Path)

	// --- Create Archive Stream ---
	log.Println("Backup: Creating git archive stream...")
	// Use -C repoPath to ensure git runs in the correct directory
	cmdArchive := exec.Command("git", "-C", repoPath, "archive", "--format=tar", commitHash)
	stdoutPipe, err := cmdArchive.StdoutPipe() // Get pipe BEFORE starting command
	if err != nil {
		return fmt.Errorf("failed to get stdout pipe for git archive: %w", err)
	}
	// Capture stderr separately to show git errors if they occur
	var stderr strings.Builder
	cmdArchive.Stderr = &stderr

	// Start git archive (doesn't wait)
	if err := cmdArchive.Start(); err != nil {
		return fmt.Errorf("failed to start git archive: %w", err)
	}

	// --- Create Gzip Stream ---
	gzipReader, gzipWriter := GzipPipe(stdoutPipe) // Use a helper for clarity

	// --- Upload to S3 ---
	log.Println("Backup: Starting S3 upload...")
	_, err = s3Client.PutObject(context.TODO(), &s3.PutObjectInput{
		Bucket: aws.String(cfg.Bucket),
		Key:    aws.String(s3Key),
		Body:   gzipReader, // Read from the gzip reader pipe
		// ACL: types.ObjectCannedACLBucketOwnerFullControl, // Optional: Set ACL if needed
		// ContentType: aws.String("application/gzip"), // Optional
	})

    // Ensure the 'git archive' command finishes and check for errors AFTER upload attempt/reader close
    archiveErr := cmdArchive.Wait() // Wait for git archive to finish
    // Closing the gzipReader should signal EOF up the chain, making git archive eventually exit


	if err != nil {
		// S3 upload failed
        // Close the reader explicitly on error? Maybe handled by context cancellation implicitly.
        // gzipReader.Close() ? Depends on implementation of GzipPipe
		return fmt.Errorf("failed to upload to S3: %w", err)
	}

	// Check git archive error *after* potential S3 error
	if archiveErr != nil {
		return fmt.Errorf("git archive command failed (stderr: %s): %w", stderr.String(), archiveErr)
	}

	log.Printf("Backup: Upload seems successful for %s", s3Path)
	return nil // Success
}

// GzipPipe takes a reader (like command stdout) and returns a reader pipe
// from which gzipped data can be read. Runs compression in a goroutine.
func GzipPipe(r *exec.Cmd) (*gzipReadCloser, *gzip.Writer) {
    // Custom type to allow closing the reader end of the pipe
    type gzipReadCloser struct {
        r *os.File
        w *os.File
    }
    func (grc *gzipReadCloser) Read(p []byte) (n int, err error) {
        return grc.r.Read(p)
    }
    func (grc *gzipReadCloser) Close() error {
        // Closing the reader signals the writer potentially
        e1 := grc.r.Close()
        // Also ensure writer is closed if necessary? Depends.
        // e2 := grc.w.Close() // Closing writer might be done by gzipWriter.Close()
        if e1 != nil { return e1 }
        // if e2 != nil { return e2 }
        return nil
    }


	rPipe, wPipe, err := os.Pipe() // Read end | Write end
	if err != nil {
		log.Fatalf("Backup: Failed to create gzip pipe: %v", err) // Fatal, indicates issue
	}

    grc := &gzipReadCloser{r: rPipe, w: wPipe}

	gzipWriter := gzip.NewWriter(wPipe) // Write gzipped data *to* the pipe's write end

	// Goroutine to read from input -> gzip -> write to pipe
	go func() {
        // Close the things this goroutine owns when done
		defer wPipe.Close()      // Close pipe writer when done
		defer gzipWriter.Close() // Close gzip writer to flush data

        // Read from the command's stdout pipe provided as input 'r'
        stdoutPipe, err := r.StdoutPipe()
        if err != nil {
            log.Printf("Backup: Error getting command stdout pipe in goroutine: %v", err)
            return
        }

		_, err = gzipWriter.ReadFrom(stdoutPipe) // Read from cmd stdout -> gzip -> pipe writer
		if err != nil {
			log.Printf("Backup: Error during gzip compression/pipe write: %v", err)
		}
        log.Println("Backup: Gzip goroutine finished.")

	}()

	// Return the *reader* end of the pipe
	return grc, gzipWriter
}


// GzipPipe (Alternative simpler approach if direct piping works)
// func GzipPipe(r io.ReadCloser) io.ReadCloser {
//     pr, pw := io.Pipe()
//     gw := gzip.NewWriter(pw)
//     go func() {
//         defer r.Close()
//         defer pw.Close() // Close writer when copying is done
//         defer gw.Close() // Close gzip writer to flush
//         _, err := io.Copy(gw, r)
//         if err != nil {
//              log.Printf("Backup: Error during gzip pipe copy: %v", err)
//              // Need to close pw with error? io.Pipe handles this.
//              pw.CloseWithError(fmt.Errorf("gzip copy failed: %w", err))
//         }
//          log.Println("Backup: Gzip goroutine finished.")
//     }()
//     return pr // Return reader end
// }