package registry

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"os"

	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	bspb "google.golang.org/genproto/googleapis/bytestream"
	"k8s.io/klog"
)

// CASClient reads blobs from Buildbarn's CAS via the ByteStream and REAPI gRPC APIs.
type CASClient struct {
	conn         *grpc.ClientConn
	client       bspb.ByteStreamClient
	cas          repb.ContentAddressableStorageClient
	instanceName string
}

// NewCASClient dials addr with no transport security (for local/dev use).
func NewCASClient(addr, instanceName string) (*CASClient, error) {
	return newCASClient(addr, instanceName, insecure.NewCredentials())
}

// NewCASClientTLS dials addr using mTLS. certFile/keyFile are the client
// certificate and key; caFile is the CA bundle used to verify the server.
func NewCASClientTLS(addr, instanceName, certFile, keyFile, caFile string) (*CASClient, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("loading CAS client cert: %w", err)
	}
	caData, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("reading CAS CA cert: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caData) {
		return nil, fmt.Errorf("parsing CAS CA cert: no valid PEM block found")
	}
	creds := credentials.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      pool,
	})
	return newCASClient(addr, instanceName, creds)
}

func newCASClient(addr, instanceName string, creds credentials.TransportCredentials) (*CASClient, error) {
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(creds))
	if err != nil {
		return nil, fmt.Errorf("dialing CAS at %s: %w", addr, err)
	}
	klog.Infof("registry: CAS client configured for %s (instance=%q)", addr, instanceName)
	return &CASClient{
		conn:         conn,
		client:       bspb.NewByteStreamClient(conn),
		cas:          repb.NewContentAddressableStorageClient(conn),
		instanceName: instanceName,
	}, nil
}

// ReadBlob streams the blob identified by its hex hash and size into w.
func (c *CASClient) ReadBlob(ctx context.Context, hash string, size int64, w io.Writer) error {
	resourceName := fmt.Sprintf("blobs/%s/%d", hash, size)
	if c.instanceName != "" {
		resourceName = fmt.Sprintf("%s/blobs/%s/%d", c.instanceName, hash, size)
	}

	stream, err := c.client.Read(ctx, &bspb.ReadRequest{
		ResourceName: resourceName,
	})
	if err != nil {
		return fmt.Errorf("bytestream read %s: %w", resourceName, err)
	}

	for {
		resp, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("bytestream recv: %w", err)
		}
		if _, err := w.Write(resp.Data); err != nil {
			return fmt.Errorf("writing blob data: %w", err)
		}
	}
}

// FindMissingBlobs checks which of the given {digest → size} pairs are absent
// from CAS and returns their digests. An empty slice means all blobs are present.
func (c *CASClient) FindMissingBlobs(ctx context.Context, blobs []BlobMeta) ([]BlobMeta, error) {
	digests := make([]*repb.Digest, 0, len(blobs))
	for _, b := range blobs {
		digests = append(digests, &repb.Digest{
			Hash:      b.Hash,
			SizeBytes: b.Size,
		})
	}
	resp, err := c.cas.FindMissingBlobs(ctx, &repb.FindMissingBlobsRequest{
		InstanceName: c.instanceName,
		BlobDigests:  digests,
	})
	if err != nil {
		return nil, fmt.Errorf("FindMissingBlobs: %w", err)
	}
	missing := make([]BlobMeta, 0, len(resp.MissingBlobDigests))
	for _, d := range resp.MissingBlobDigests {
		missing = append(missing, BlobMeta{Hash: d.Hash, Size: d.SizeBytes})
	}
	return missing, nil
}

// Close releases the underlying gRPC connection.
func (c *CASClient) Close() error {
	return c.conn.Close()
}
