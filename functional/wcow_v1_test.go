// +build functional wcow wcowv1

package functional

// Has testing for v1 Windows containers using both the older hcsshim methods,
// and the newer hcsoci methods. Each test does the same thing in six different
// ways:
//    - hcsshim/xenon
//    - hcsshim/argon
//    - hcsoci/xenon v1
//    - hcsoci/argon v1
//    - hcsoci/xenon v2
//    - hcsoci/argon v2
//
// Typical v1 HCS document for Xenon (no networking):
//
//{
//    "SystemType": "Container",
//    "Name": "48347b95d0ad4f37de6d1979b986fb65912f973ad4549fbe716e848679dfa25c",
//    "IgnoreFlushesDuringBoot": true,
//    "LayerFolderPath": "C:\\control\\windowsfilter\\48347b95d0ad4f37de6d1979b986fb65912f973ad4549fbe716e848679dfa25c",
//    "Layers": [
//        {
//            "ID": "7095521e-b79e-50fc-bafb-958d85400362",
//            "Path": "C:\\control\\windowsfilter\\f9b22d909166dd54b870eb699d54f4cf36d99f035ffd7701aff1267230aefd1e"
//        }
//    ],
//    "HvPartition": true,
//    "HvRuntime": {
//        "ImagePath": "C:\\control\\windowsfilter\\f9b22d909166dd54b870eb699d54f4cf36d99f035ffd7701aff1267230aefd1e\\UtilityVM"
//    }
//}
// Typical v1 HCS document for Argon (no networking):
//{
//    "SystemType": "Container",
//    "Name": "0a8bb9ec8366aa48a8e5f810274701d8d4452989bf268fc338570bfdecddf8df",
//    "VolumePath": "\\\\?\\Volume{85da95c9-dda9-42e0-a066-40bd120c6f3c}",
//    "IgnoreFlushesDuringBoot": true,
//    "LayerFolderPath": "C:\\control\\windowsfilter\\0a8bb9ec8366aa48a8e5f810274701d8d4452989bf268fc338570bfdecddf8df",
//    "Layers": [
//        {
//            "ID": "7095521e-b79e-50fc-bafb-958d85400362",
//            "Path": "C:\\control\\windowsfilter\\f9b22d909166dd54b870eb699d54f4cf36d99f035ffd7701aff1267230aefd1e"
//        }
//    ],
//    "HvPartition": false
//}

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Microsoft/hcsshim"
	"github.com/Microsoft/hcsshim/functional/utilities"
	"github.com/Microsoft/hcsshim/internal/hcs"
	"github.com/Microsoft/hcsshim/internal/hcsoci"
	"github.com/Microsoft/hcsshim/internal/schemaversion"
	"github.com/Microsoft/hcsshim/internal/uvmfolder"
	"github.com/Microsoft/hcsshim/internal/wclayer"
	specs "github.com/opencontainers/runtime-spec/specs-go"
)

// Helper to start a container.
// Ones created through hcsoci methods will be of type *hcs.System.
// Ones created through hcsshim methods will be of type hcsshim.Container
func startContainer(t *testing.T, c interface{}) {
	var err error
	switch c.(type) {
	case *hcs.System:
		err = c.(*hcs.System).Start()
	case hcsshim.Container:
		err = c.(hcsshim.Container).Start()
	default:
		t.Fatal("unknown type")
	}
	if err != nil {
		t.Fatalf("Failed start: %s", err)
	}
}

// Helper to stop a container.
// Ones created through hcsoci methods will be of type *hcs.System.
// Ones created through hcsshim methods will be of type hcsshim.Container
func stopContainer(t *testing.T, c interface{}) {

	switch c.(type) {
	case *hcs.System:
		if err := c.(*hcs.System).Shutdown(); err != nil {
			if hcsshim.IsPending(err) {
				if err := c.(*hcs.System).Wait(); err != nil {
					t.Fatalf("Failed Wait shutdown: %s", err)
				}
			} else {
				t.Fatalf("Failed shutdown: %s", err)
			}
		}
		c.(*hcs.System).Terminate()

	case hcsshim.Container:
		if err := c.(hcsshim.Container).Shutdown(); err != nil {
			if hcsshim.IsPending(err) {
				if err := c.(hcsshim.Container).Wait(); err != nil {
					t.Fatalf("Failed Wait shutdown: %s", err)
				}
			} else {
				t.Fatalf("Failed shutdown: %s", err)
			}
		}
		c.(hcsshim.Container).Terminate()
	default:
		t.Fatalf("unknown type")
	}
}

// Helper to launch a process in a container created through the hcsshim methods.
// At the point of calling, the container must have been successfully created.
func runShimCommand(t *testing.T, c hcsshim.Container, command, workdir, expectedOutput string) {
	if c == nil {
		t.Fatalf("requested container to start is nil!")
	}
	p, err := c.CreateProcess(&hcsshim.ProcessConfig{
		CommandLine:      command,
		WorkingDirectory: workdir,
		CreateStdInPipe:  true,
		CreateStdOutPipe: true,
		CreateStdErrPipe: true,
	})
	if err != nil {
		t.Fatalf("Failed Create Process: %s", err)

	}
	defer p.Close()
	if err := p.Wait(); err != nil {
		t.Fatalf("Failed Wait Process: %s", err)
	}
	exitCode, err := p.ExitCode()
	if err != nil {
		t.Fatalf("Failed to obtain process exit code: %s", err)
	}
	if exitCode != 0 {
		t.Fatalf("Non-zero exit code from process %s (%d)", command, exitCode)
	}
	_, o, _, err := p.Stdio()
	if err != nil {
		t.Fatalf("Failed to get Stdio handles for process: %s", err)
	}
	buf := new(bytes.Buffer)
	buf.ReadFrom(o)
	out := strings.TrimSpace(buf.String())
	if out != expectedOutput {
		t.Fatalf("Failed to get %q from process: %q", expectedOutput, out)
	}
	fmt.Println("Got", out)
}

func TestWCOWV1(t *testing.T) {
	imageLayers := testutilities.LayerFolders(t, "busybox")

	// Container scratches
	xenonShimScratchDir := testutilities.CreateTempDir(t)
	defer os.RemoveAll(xenonShimScratchDir)
	if err := wclayer.CreateScratchLayer(xenonShimScratchDir, imageLayers); err != nil {
		t.Fatalf("failed to create xenon scratch layer: %s", err)
	}

	argonShimScratchDir := testutilities.CreateTempDir(t)
	defer os.RemoveAll(argonShimScratchDir)
	if err := wclayer.CreateScratchLayer(argonShimScratchDir, imageLayers); err != nil {
		t.Fatalf("failed to create argon scratch layer: %s", err)
	}

	xenonOciScratchDir := testutilities.CreateTempDir(t)
	defer os.RemoveAll(xenonOciScratchDir)
	if err := wclayer.CreateScratchLayer(xenonOciScratchDir, imageLayers); err != nil {
		t.Fatalf("failed to create xenon scratch layer: %s", err)
	}

	argonOciScratchDir := testutilities.CreateTempDir(t)
	defer os.RemoveAll(argonOciScratchDir)
	if err := wclayer.CreateScratchLayer(argonOciScratchDir, imageLayers); err != nil {
		t.Fatalf("failed to create argon scratch layer: %s", err)
	}

	uvmImagePath, err := uvmfolder.LocateUVMFolder(imageLayers)
	if err != nil {
		t.Fatalf("LocateUVMFolder failed %s", err)
	}

	var layers []hcsshim.Layer
	for _, layerFolder := range imageLayers {
		guid, _ := wclayer.NameToGuid(filepath.Base(layerFolder))
		layers = append(layers, hcsshim.Layer{Path: layerFolder, ID: guid.String()})
	}

	//
	// 1. Xenon through hcsshim
	//

	xenonShim, err := hcsshim.CreateContainer("xenon", &hcsshim.ContainerConfig{
		SystemType:      "Container",
		Name:            "xenonShim",
		LayerFolderPath: xenonShimScratchDir,
		Layers:          layers,
		HvRuntime:       &hcsshim.HvRuntime{ImagePath: uvmImagePath},
	})
	if err != nil {
		t.Fatal(err)
	}
	startContainer(t, xenonShim)
	runShimCommand(t, xenonShim, "cmd /s /c echo XenonShim", `c:\`, "XenonShim")
	stopContainer(t, xenonShim)

	//
	// 2. Argon through hcsshim
	//

	// This is a cheat but stops us re-writing exactly the same code just for test
	argonShimLocalMountPath, err := hcsoci.MountContainerLayers(append(imageLayers, argonShimScratchDir), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer hcsoci.UnmountContainerLayers(append(imageLayers, argonShimScratchDir), "", nil, hcsoci.UnmountOperationAll)
	argonShim, err := hcsshim.CreateContainer("argon", &hcsshim.ContainerConfig{
		SystemType:      "Container",
		Name:            "argonShim",
		VolumePath:      argonShimLocalMountPath.(string),
		LayerFolderPath: argonShimScratchDir,
		Layers:          layers,
		HvRuntime:       nil,
	})
	if err != nil {
		t.Fatal(err)
	}
	startContainer(t, argonShim)
	runShimCommand(t, argonShim, "cmd /s /c echo ArgonShim", `c:\`, "ArgonShim")
	stopContainer(t, argonShim)

	//
	// 3. Xenon through hcsoci
	//

	xenonOci, xenonOciResources, err := hcsoci.CreateContainer(
		&hcsoci.CreateOptions{
			ID:            "xenonOci",
			SchemaVersion: schemaversion.SchemaV10(),
			Spec: &specs.Spec{
				Windows: &specs.Windows{
					LayerFolders: append(imageLayers, xenonOciScratchDir),
					HyperV:       &specs.WindowsHyperV{},
				},
			},
		})
	if err != nil {
		t.Fatal(err)
	}
	defer hcsoci.ReleaseResources(xenonOciResources, nil, true)
	startContainer(t, xenonOci)
	// TODO: run/terminate

	//
	// 4. Argon through hcsoci
	//

	argonOci, argonOciResources, err := hcsoci.CreateContainer(
		&hcsoci.CreateOptions{
			ID:            "argonOci",
			SchemaVersion: schemaversion.SchemaV10(),
			Spec:          &specs.Spec{Windows: &specs.Windows{LayerFolders: append(imageLayers, argonOciScratchDir)}},
		})
	if err != nil {
		t.Fatal(err)
	}
	defer hcsoci.ReleaseResources(argonOciResources, nil, true)
	startContainer(t, argonOci)
	// TODO: run/terminate

}

//// A v1 WCOW Xenon with a single base layer but let HCSShim find the utility VM path
//func TestV1XenonWCOWNoUVMPath(t *testing.T) {
//	t.Skip("for now")
//	tempDir := createWCOWTempDirWithSandbox(t)
//	defer os.RemoveAll(tempDir)

//	c, err := CreateContainer(&CreateOptions{
//		Id:            "TestV1XenonWCOWNoUVMPath",
//		Owner:         "unit-test",
//		SchemaVersion: schemaversion.SchemaV10(),
//		Spec: &specs.Spec{
//			Windows: &specs.Windows{
//				LayerFolders: append(layersNanoserver, tempDir),
//				HyperV:       &specs.WindowsHyperV{},
//			},
//		},
//	})
//	if err != nil {
//		t.Fatalf("Failed create: %s", err)
//	}
//	startContainer(t, c)
//	runCommand(t, c, "cmd /s /c echo Hello", `c:\`, "Hello")
//	stopContainer(t, c)
//}

//// A v1 WCOW Xenon with multiple layers letting HCSShim find the utilityVM Path
//func TestV1XenonMultipleBaseLayersNoUVMPath(t *testing.T) {
//	t.Skip("for now")
//	tempDir := createWCOWTempDirWithSandbox(t)
//	defer os.RemoveAll(tempDir)

//	layers := layersBusybox
//	c, err := CreateContainer(&CreateOptions{
//		Id:            "TestV1XenonWCOW",
//		SchemaVersion: schemaversion.SchemaV10(),
//		Spec: &specs.Spec{
//			Windows: &specs.Windows{
//				LayerFolders: append(layers, tempDir),
//				HyperV:       &specs.WindowsHyperV{},
//			},
//		},
//	})
//	if err != nil {
//		t.Fatalf("Failed create: %s", err)
//	}
//	startContainer(t, c)
//	runCommand(t, c, "cmd /s /c echo Hello", `c:\`, "Hello")
//	stopContainer(t, c)
//}

//// TestV1XenonWCOWSingleMappedDirectory tests a V1 Xenon WCOW with a single mapped directory
//func TestV1XenonWCOWSingleMappedDirectory(t *testing.T) {
//	t.Skip("Skipping for now")

//	containerScratchDir := createWCOWTempDirWithSandbox(t)
//	defer os.RemoveAll(containerScratchDir)
//	layerFolders := append(layersNanoserver, containerScratchDir)

//	// Create a temp folder containing foo.txt which will be used for the bind-mount test.
//	source := createTempDir(t)
//	defer os.RemoveAll(source)
//	mount := specs.Mount{
//		Source:      source,
//		Destination: `c:\foo`,
//	}
//	f, err := os.OpenFile(filepath.Join(source, "foo.txt"), os.O_RDWR|os.O_CREATE, 0755)
//	f.Close()

//	tempDir := createWCOWTempDirWithSandbox(t)
//	defer os.RemoveAll(tempDir)

//	hostedContainer, err := CreateContainer(&CreateOptions{
//		Id:            "TestV1XenonWCOWSingleMappedDirectory",
//		SchemaVersion: schemaversion.SchemaV10(),
//		Spec: &specs.Spec{
//			Mounts: []specs.Mount{mount},
//			Windows: &specs.Windows{
//				LayerFolders: layerFolders,
//				HyperV:       &specs.WindowsHyperV{},
//			},
//		},
//	})
//	if err != nil {
//		t.Fatalf("Failed create: %s", err)
//	}

//	// Start/stop the container
//	startContainer(t, hostedContainer)
//	runCommand(t, hostedContainer, `cmd /s /c dir /b c:\foo`, `c:\`, "foo.txt")
//	stopContainer(t, hostedContainer)
//	hostedContainer.Terminate()
//}
