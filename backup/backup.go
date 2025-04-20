// backup/backup.go
package backup

import (
	"compress/gzip"
	"context"
	"fmt"
	"io" // Import io package
	"log"
	"os" // Import os package
	"os/exec"
	"strings"

	// Use the actual module path defined in your go.mod file
	// Example: "github.com/yourusername/git-monitor-app/config"
	// Adjust this path if your module name is different:
	"git-monitor-app/config"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsConfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// --- Gzip Pipe Helper ---

// gzipReadCloser wraps the reader end of an os.Pipe and implements io.ReadCloser
type gzipReadCloser struct {
	pr *os.File // The pipe's reader end
	// We don't strictly need pw here, closing pr signals EOF to the writer side of the pipe
}

// Read implements io.Reader
func (grc *gzipReadCloser) Read(p []byte) (n int, err error) {
	n, err = grc.pr.Read(p)
	return n, err // Return results directly
}

// Close implements io.Closer
func (grc *gzipReadCloser) Close() error {
	// Closing the reader end signals the goroutine (via io.Copy eventually)
	// and helps clean up resources.
	return grc.pr.Close()
}

// GzipPipe sets up a pipeline where data read from 'src' is gzipped
// and can be read from the returned io.ReadCloser. Compression happens
// in a background goroutine.
func GzipPipe(src io.Reader) (io.ReadCloser, error) {
	rPipe, wPipe, err := os.Pipe() // Read end | Write end for compressed data
	if err != nil {
		return nil, fmt.Errorf("failed to create gzip pipe: %w", err)
	}

	grc := &gzipReadCloser{pr: rPipe} // Our custom type holding the reader pipe end

	gzipWriter := gzip.NewWriter(wPipe) // gzip writes TO the pipe's writer end

	// Goroutine to read from input source -> gzip -> write to pipe
	go func() {
		// IMPORTANT: Close the pipe writer and gzip writer when done copying
		// This signals EOF or errors to the reader end (grc)
		defer wPipe.Close()
		defer gzipWriter.Close() // Ensures compressed data is flushed

		// Copy data from the source (e.g., command stdout) to the gzip writer
		_, copyErr := io.Copy(gzipWriter, src)
		if copyErr != nil {
			// Error during copy. Propagate the error to the reader end.
			log.Printf("Backup Error: during gzip pipe copy: %v", copyErr)
			// Close the writer end with the error to propagate it to the reader
			// The reader (grc.Read) will receive this error on its next Read call.
			wPipe.CloseWithError(copyErr)
		}
		log.Println("Backup: Gzip goroutine finished.")
	}()

	// Return the *reader* end of the pipe, wrapped in our closer type
	return grc, nil
}

// --- Backup Functionality ---

// RunBackup performs the backup of a specific commit to S3/Wasabi.
func RunBackup(repoPath, commitHash string, cfg *config.BackupConfig) error {
	log.Printf("Backup: Starting backup process for commit %s", commitHash)

	// Basic validation of essential config
	if cfg.Bucket == "" {
		return fmt.Errorf("backup config error: S3 bucket name is required")
	}

	// --- Configure AWS SDK ---
	log.Println("Backup: Configuring S3 client...")
	sdkConfigOptions := []func(*awsConfig.LoadOptions) error{}

	// 1. Custom Endpoint Resolver (Essential for Wasabi/S3 Compatible)
	if cfg.EndpointURL != "" {
		customResolver := aws.EndpointResolverWithOptionsFunc( // Use newer resolver type
			func(service, region string, options ...interface{}) (aws.Endpoint, error) {
				if service == s3.ServiceID {
					ep := aws.Endpoint{
						URL:           cfg.EndpointURL,
						SigningRegion: cfg.Region, // Use region from config for signing
					}
					// You might need to set PartitionID or other fields depending on the S3 provider
					return ep, nil
				}
				// Fallback to default AWS resolution for other services (if any)
				return aws.Endpoint{}, &aws.EndpointNotFoundError{}
			})
		sdkConfigOptions = append(sdkConfigOptions, awsConfig.WithEndpointResolverWithOptions(customResolver))
	}

	// 2. Region (Needed if not AWS default and no custom endpoint, or if endpoint needs signing region)
	if cfg.Region != "" {
		sdkConfigOptions = append(sdkConfigOptions, awsConfig.WithRegion(cfg.Region))
	}

	// 3. Credentials (Prioritize explicit config over default chain if provided)
	if cfg.AccessKeyID != "" && cfg.SecretKey != "" {
		log.Println("Backup: Using static credentials from config file.")
		staticCreds := credentials.NewStaticCredentialsProvider(cfg.AccessKeyID, cfg.SecretKey, "") // Empty session token
		sdkConfigOptions = append(sdkConfigOptions, awsConfig.WithCredentialsProvider(staticCreds))
	} else {
		log.Println("Backup: Using default AWS credential chain (Env Vars, Shared Config/Credentials).")
		// Default behavior of LoadDefaultConfig uses the chain
	}

	// Load the final configuration
	sdkConfig, err := awsConfig.LoadDefaultConfig(context.TODO(), sdkConfigOptions...)
	if err != nil {
		return fmt.Errorf("failed to load AWS SDK config: %w", err)
	}
	// Ensure region is set correctly if needed after loading defaults, especially if resolver didn't set it
	if cfg.Region != "" && sdkConfig.Region != cfg.Region {
		sdkConfig.Region = cfg.Region
		log.Printf("Backup: Explicitly setting region in loaded SDK config: %s", cfg.Region)
	}

	// Create S3 client
	s3Client := s3.NewFromConfig(sdkConfig)

	// --- Construct S3 Key ---
	backupFilename := fmt.Sprintf("commit-%s.tar.gz", commitHash)
	s3Key := backupFilename
	if cfg.Prefix != "" {
		cleanPrefix := strings.Trim(cfg.Prefix, "/")
		if cleanPrefix != "" {
			s3Key = fmt.Sprintf("%s/%s", cleanPrefix, backupFilename)
		}
	}
	s3Path := fmt.Sprintf("s3://%s/%s", cfg.Bucket, s3Key)
	log.Printf("Backup: Target S3 Key: %s\n", s3Path)

	// --- Create Archive Stream ---
	log.Println("Backup: Creating git archive stream...")
	cmdArchive := exec.Command("git", "-C", repoPath, "archive", "--format=tar", commitHash)
	stdoutPipe, err := cmdArchive.StdoutPipe() // Get pipe BEFORE starting command
	if err != nil {
		return fmt.Errorf("failed to get stdout pipe for git archive: %w", err)
	}
	var stderr strings.Builder // Capture stderr for better error messages
	cmdArchive.Stderr = &stderr

	// Start git archive (doesn't wait)
	if err := cmdArchive.Start(); err != nil {
		return fmt.Errorf("failed to start git archive: %w", err)
	}

	// --- Setup Gzip Pipe ---
	// Pass the command's stdout directly to the GzipPipe helper
	gzipReader, err := GzipPipe(stdoutPipe)
	if err != nil {
		// If pipe creation failed, try to kill the command? Or just return error.
		// Ensure command resources are cleaned up if possible
		_ = cmdArchive.Process.Kill() // Attempt to clean up started process
		_ = cmdArchive.Wait()         // Wait to release resources associated with the command
		return fmt.Errorf("failed to setup gzip pipe: %w", err)
	}
	// Ensure the gzipReader pipe is closed eventually to release resources
	// Closing it will also signal EOF/error up the chain to the command/goroutine
	defer gzipReader.Close()

	// --- Upload to S3 ---
	log.Println("Backup: Starting S3 upload...")
	_, uploadErr := s3Client.PutObject(context.TODO(), &s3.PutObjectInput{
		Bucket: aws.String(cfg.Bucket),
		Key:    aws.String(s3Key),
		Body:   gzipReader, // Read directly from the gzip reader pipe
		// ContentType: aws.String("application/gzip"), // Optional: Helps S3 understand the content
		// ACL, Metadata etc. can be added here if needed
	})

	// Wait for the 'git archive' command to finish *after* upload attempt (or failure)
	// Reading from gzipReader (which happens inside PutObject) will block until git archive finishes or errors.
	// Closing gzipReader signals EOF upstream.
	archiveErr := cmdArchive.Wait()

	// Check for errors, prioritizing S3 upload error
	if uploadErr != nil {
		// S3 upload failed
		// Log git archive error if it also happened
		if archiveErr != nil {
			log.Printf("Backup: git archive command also failed (stderr: %s): %v", stderr.String(), archiveErr)
		}
		return fmt.Errorf("failed to upload to S3 (%s): %w", s3Path, uploadErr)
	}

	// Check git archive error if S3 upload seemed okay
	if archiveErr != nil {
		return fmt.Errorf("git archive command failed after successful upload (stderr: %s): %w", stderr.String(), archiveErr)
	}

	log.Printf("Backup: Upload SUCCEEDED for %s", s3Path)
	return nil // Success
}
