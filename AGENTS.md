# Working on this fork

[README.md](README.md) describes what the fork changes and how to deploy it.
This file is how to build, test, pin `weed`, merge and release.

## Build and test

```sh
go vet ./pkg/... ./cmd/...
go test -race -count=1 ./...
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build ./...
GOOS=windows GOARCH=amd64 go vet ./pkg/... ./cmd/...
python3 -m unittest discover -s test/kubernetes_lab -p 'test_*.py' -v
python3 -m unittest discover -s hack/release -p 'test_*.py' -v
hack/appmana/tests/run-appmana-script-tests.sh
```

This is what `build-images.yml` runs before it builds images; the native
Windows job also runs `./pkg/driver/` and `./pkg/mountmanager/` on a Windows
runner and fails if a required test did not run. With an older local Go than
`go.mod`, set `GOTOOLCHAIN=auto`.

`pkg/driver` `TestNodeGetVolumeStats` fails when `TMPDIR` is on btrfs, which
reports 0 inodes, so the test finds no INODES usage entry. Run it with a
tmpfs `TMPDIR` (for example under `/dev/shm`).

## Where weed is pinned

The mount images build `weed` from a commit of
[forks-seaweedfs](https://github.com/AppMana/forks-seaweedfs), and the driver
compiles against the same commit's API. Pinned commits must stay reachable in
that repository. To move to a new commit, change all of these together:

- `.github/workflows/build-images.yml`: `SEAWEEDFS_COMMIT` (Linux) and
  `SEAWEEDFS_WINDOWS_COMMIT`.
- The `ARG SEAWEEDFS_COMMIT` default in `cmd/seaweedfs-mount/Dockerfile`,
  `Dockerfile.Windows` and `Dockerfile.dev`, and `GO_FUSE_COMMIT` there when
  the fork's go-fuse pin changes.
- `test/kubernetes_lab/build_pins_test.go`: the expected pins. It checks that
  the workflow and the three Dockerfiles agree, that `weed` is a static
  (`CGO_ENABLED=0`) `5BytesOffset` build stamped with
  `version.COMMIT`, and that the workflow never overwrites released tags.
- `go.mod`: `go mod edit -replace github.com/seaweedfs/seaweedfs=github.com/AppMana/forks-seaweedfs@<sha>`,
  then `go mod tidy` and the tests above.

Commit the re-pin as one change (`mount: weed <short sha> (<branch>) on Linux
and Windows`).

## Kubernetes lab

`test/kubernetes_lab/run.sh` runs the CSI driver in a fresh mixed Linux and
Windows k0s cluster of Labcontainers VMs, using the Calico fork's lab fixture.
Required inputs (by name): `CALICO_LAB_MODULE`, `CSI_LAB_ARTIFACTS`,
`LABCONTAINERS_STATE_DIR`, `LABCONTAINERS_CALICO_MEDIA` and
`LABCONTAINERS_CALICO_MEDIA_SHA256` (offline ISO with the CSI images and
`mount-smoke.ps1`), `LABCONTAINERS_VM_IMAGE`, `LABCONTAINERS_WINDOWS_IMAGE`
and `LABCONTAINERS_LABD`. Optional:

- `CSI_DRIVER_{LINUX,WINDOWS}_IMAGE`, `CSI_MOUNT_{LINUX,WINDOWS}_IMAGE`: all
  four digest-pinned images, or none.
- `CSI_QUALIFICATION_MODE`: `full` (default, the native Windows suite,
  marker `CSI_QUALIFICATION_COMPLETE`) or `recovery` (restart, remount,
  reboot and crash recovery with Git LFS, mixed I/O and persistence checks,
  marker `CSI_RECOVERY_QUALIFICATION_COMPLETE`; needs the four images).
- `LABCONTAINERS_KUBERNETES_CNI`: `calico-vxlan` (default) or `calico-bgp`.
- `LABCONTAINERS_KUBERNETES_CRASH_VERIFY=1`: verify the dataset around VM
  power loss.
- `CSI_WINFSP_DLL_SHA256` with `CSI_NATIVE_TEST_SHA256`: DLL-only lane. The
  matching native test executable goes at `/winfsp-csi-candidate.test.exe` on
  the ISO; the stock driver's signature and disabled test signing are checked.
- `CSI_CANDIDATE_BUNDLE`: kernel driver candidate lane from
  `stage_split_media.py`; it pins its own images, so do not combine it with
  the image or DLL inputs.

The mount binary's own gates (unit, `test/fuse_integration`,
`test/storage_lab`) run in forks-seaweedfs.

Packaging already-tested executables without rebuilding them:
`test/kubernetes_lab/package_images.py --inputs <dir> --sha256-file <file> --results-root <dir> --builder <buildx builder>`
takes `weed[.exe]`, `seaweedfs-mount[.exe]`, `seaweedfs-csi-driver[.exe]` and
`winfsp.msi` with a `sha256sum` file of plain names, builds the four images
through the Dockerfiles' `PAYLOAD_STAGE=prebuilt` path, checks bytes,
platform and entrypoint, smoke-tests Linux startup and keeps OCI archives,
digests and logs. `--component csi-driver` builds only the driver images;
`--winfsp-dll` adds `winfsp-x64.dll` to the mount image. It never pushes.
`test/kubernetes_lab/publish_candidate.py --archive <tar> --record <json> --component <mount|csi-driver> --results-root <dir> [--publish]`
pushes such an archive as `candidate-<digest>` after checking it against its
packaging record; it cannot write a release tag. Credentials come from
`crane`'s normal configuration.

`hack/appmana/` holds the older kind and QEMU Windows e2e scripts; see its
README.

## CI

- `build-images.yml`: pushes and pull requests to `appmana/windows-v1.4.12`
  and `merge/**`. Publishes `candidate-<sha>-<run>-<attempt>` tags only.
- `lab-contracts.yml`: pull requests, `wip/**` pushes and dispatch; builds
  both platforms and runs the race and packaging contracts without
  publishing.
- `promote-release.yml`: dispatch only, see below.

## Release

1. Add a `## v<upstream base>-appmana.post.<n>` section at the top of
   RELEASES.md naming the candidate's full source SHA, and push it to the
   default branch. `hack/release/promote.py` refuses a release whose section
   is missing or does not name that SHA.
2. Dispatch `promote-release.yml` with the release and candidate tags.
3. Add the pins it prints (also in the run's step summary) to the section's
   digest table.

Each section keeps the source commit, candidate tag, the weed, go-fuse,
WinFsp and DLL it ships, what changed, compatibility between mount and node
plugin versions, and the digest table. The counter restarts at 1 when the
fork merges a new upstream release.

## Merging upstream

- Merge, never rebase, on a `merge/upstream-*` branch; it reaches the default
  branch by merge once its images pass. The default branch is the source of
  deployed images.
- After the merge, rerun the tests above and the build pin test, and check
  that the fork's node plugin changes (staging rediscovery, health monitor,
  cache sweep, quarantine on unpublish) and per-volume parameters survived.
- Every fix gets a regression test shown failing first; no skips or xfails.

## Commits

- Concise subjects prefixed with the area (`mount:`, `node plugin:`,
  `mount service:`, `lab:`, `RELEASES.md:`).
- Reasoning goes in commit messages, not code comments or docs.
- This repository is public: docs carry no internal hostnames, addresses or
  incident references.
