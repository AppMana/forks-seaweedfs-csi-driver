package driver

import (
	"context"
	"net"
	"sync"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/seaweedfs/seaweedfs/weed/pb"
	"github.com/seaweedfs/seaweedfs/weed/pb/filer_pb"
	"github.com/seaweedfs/seaweedfs/weed/s3api/s3_constants"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/proto"
)

type createVolumeFilerServer struct {
	filer_pb.UnimplementedSeaweedFilerServer
	mu      sync.Mutex
	created []*filer_pb.CreateEntryRequest
}

func (s *createVolumeFilerServer) CreateEntry(_ context.Context, req *filer_pb.CreateEntryRequest) (*filer_pb.CreateEntryResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.created = append(s.created, proto.Clone(req).(*filer_pb.CreateEntryRequest))
	return &filer_pb.CreateEntryResponse{}, nil
}

func (s *createVolumeFilerServer) creates() []*filer_pb.CreateEntryRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*filer_pb.CreateEntryRequest(nil), s.created...)
}

func newCreateVolumeTestController(t *testing.T) (*ControllerServer, *createVolumeFilerServer) {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	server := pb.NewGrpcServer()
	filerServer := &createVolumeFilerServer{}
	filer_pb.RegisterSeaweedFilerServer(server, filerServer)
	go server.Serve(listener)
	t.Cleanup(server.Stop)

	driver := &SeaweedFsDriver{
		filers: []pb.ServerAddress{
			pb.NewServerAddressWithGrpcPort("127.0.0.1:1", listener.Addr().(*net.TCPAddr).Port),
		},
		grpcDialOption: grpc.WithTransportCredentials(insecure.NewCredentials()),
		signature:      1,
	}
	driver.AddControllerServiceCapabilities([]csi.ControllerServiceCapability_RPC_Type{
		csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME,
	})
	return NewControllerServer(driver), filerServer
}

// A bucket created for a volume must keep its empty directories: the filer's
// empty-folder cleaner otherwise removes a directory between mkdir and the
// first file landing in it.
func TestCreateVolumeMarksBucketToKeepEmptyFolders(t *testing.T) {
	controller, filerServer := newCreateVolumeTestController(t)

	resp, err := controller.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name: "pvc-1234",
		VolumeCapabilities: []*csi.VolumeCapability{{
			AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{}},
			AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER},
		}},
	})
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	if resp.GetVolume().GetVolumeId() != "/buckets/pvc-1234" {
		t.Fatalf("volume id = %q, want /buckets/pvc-1234", resp.GetVolume().GetVolumeId())
	}

	creates := filerServer.creates()
	if len(creates) != 1 {
		t.Fatalf("expected one CreateEntry, got %d", len(creates))
	}
	entry := creates[0].GetEntry()
	if creates[0].GetDirectory() != "/buckets" || entry.GetName() != "pvc-1234" || !entry.GetIsDirectory() {
		t.Fatalf("unexpected bucket create: %v", creates[0])
	}
	if got := string(entry.GetExtended()[s3_constants.ExtAllowEmptyFolders]); got != "true" {
		t.Fatalf("bucket must be created with %s=true, got %q", s3_constants.ExtAllowEmptyFolders, got)
	}
}
