# Releases

Each release is the exact images of one CI candidate (`build-images.yml`,
`candidate-<sha>-<run>-<attempt>`) promoted by `promote-release.yml`; see
"Releases" in the README. Pins name the release and the digest:
`ghcr.io/appmana/<image>:<release>[-<platform>]@sha256:<digest>`.
Earlier releases (`v1.4.12-appmana.post.*`, `v1.4.26-appmana.post.*`)
predate this file.

## v1.4.34-appmana.post.2

Source `73a931aa3eed8f1a13d61d0e062225ee4d3591aa`, candidate
`candidate-73a931aa3eed8f1a13d61d0e062225ee4d3591aa-40558255116-1`. Same
weed 4.48 `17806ab0557444ce3f03cc62aec5a8768b4e1885`, WinFsp 2.1.25156 and
DLL `appmana-dll-2.1-066843359` as post.1. Moves what every deployment had to
add around the driver into the driver:

- Windows mount service installs WinFsp itself before it accepts mounts:
  copies the image's `winfsp.msi` to `%SystemRoot%\Temp`, runs `msiexec /qn
  INSTALLLEVEL=1000 /l*v …`, fails startup on a non-zero exit naming the log,
  and requires the installed version to be 2.1.25156 (refuses any other, never
  upgrades). The `install-winfsp` initContainer is no longer needed.
- Node plugin defaults: `--cacheDir` is `/var/cache/seaweedfs` or
  `C:\var\cache\seaweedfs` (was the temp directory); `--stagingScanDir` is
  kubelet's default `plugins/kubernetes.io/csi` directory (was off). Set
  `--stagingScanDir` where kubelet has another root (k0s); `--stagingScanDir=`
  turns rediscovery off.
- Node plugin creates its CSI endpoint directory.
- The shared chunk cache is always `<cacheDir>/chunks`. A StorageClass or PV
  `sharedChunkCacheDir` is now ignored (logged), and a VolumeAttributesClass
  can no longer set it; a node-level `--mountExtraArgs` value still wins.
- Orphaned cache directories are swept hourly, not only at startup. Kept: the
  shared chunk cache, volumes staged by the plugin, volumes kubelet still has
  a staging directory for, volumes being staged, and caches of mounts that
  answer on their socket. A sweep that would wait for a stage skips the round.
  Replaces a separate cache GC.
- Releases: `promote-release.yml` promotes candidates to these tags.

Compatibility: either side of a node may run post.1 while the other runs
post.2 (both DaemonSets roll OnDelete). The mount request API and required
flags are unchanged; a post.2 node plugin's `-sharedChunkCacheDir` is defined
by post.1's weed.

Qualification: CI run 40558255116 (race unit tests; native Windows tests,
including a real install of the pinned MSI through the mount service's
installer on the hosted runner). Production canary: appmana-026.

| Image | Digest |
| --- | --- |
| `seaweedfs-csi-driver:v1.4.34-appmana.post.2` | `sha256:8a6a4491706bdc16ebb12dd1712c538ff528a08d05a448a971814de90339abe7` |
| `seaweedfs-csi-driver:v1.4.34-appmana.post.2-linux-amd64` | `sha256:149c86db1c5855e68d4c4c255c8cefdd7e04bb8964ae29f00f579fb7eb82d003` |
| `seaweedfs-csi-driver:v1.4.34-appmana.post.2-windows-ltsc2022` | `sha256:23703f86590c246863732295eb2e7cd62f74596acfd0c5a7dde54e2ee6dd4711` |
| `seaweedfs-mount:v1.4.34-appmana.post.2` | `sha256:40f98c7dfdd89ff6dd8e705c4aeee3637767c3b14a420f108c8351dc28c86b5c` |
| `seaweedfs-mount:v1.4.34-appmana.post.2-linux-amd64` | `sha256:894b592f95dc281f83b1fbb5ce5995dd9c2a64f5823f2ac129928f37a5e4791c` |
| `seaweedfs-mount:v1.4.34-appmana.post.2-windows-ltsc2022` | `sha256:9e4041eb351a4d04437fbe261ac90a5f022e9ef86287704e6754841c5457fb68` |

## v1.4.34-appmana.post.1

Source `589aaadabbfa7703d23c8a39cdfaf53ac8a6750b`, candidate
`candidate-589aaadabbfa7703d23c8a39cdfaf53ac8a6750b-40541552404-1`.
Upstream base v1.4.34 (merge of upstream master at `2c85158`). Mount images
run weed 4.48 `17806ab0557444ce3f03cc62aec5a8768b4e1885` (Linux and
Windows); the Windows image ships WinFsp 2.1.25156 and the user-mode DLL
`appmana-dll-2.1-066843359` beside `weed.exe`.

Since `v1.4.26-appmana.post.2`:

- Mount service tolerates node plugin and weed flag skew: drops optional
  flags its weed does not define, fails fast naming a missing required flag,
  and reports an early weed exit at once.
- Shared node chunk cache `<cacheDir>/chunks` kept by every cleanup;
  `wholeCacheCapacityMB`, `sharedChunkCacheDir`, `readerCacheSizeMB` volume
  parameters.
- Health sweep repairs containers left on a dead mount every time (bind of
  the live staging mount, no pod deletion); a mount counts as hung only while
  one probe stays unanswered past weed's request bound.
- Node-wide write buffer cap (`--writeBufferSizeMB`), overridable per volume.
- Cache dirs of staged and served mounts survive node plugin startup; a
  mount's start removes its stale instance caches.
- Windows publishes record where pods mount each volume
  (`-winfspMountRoots`) so absolute symlinks resolve.
- Publish paths of statically provisioned volumes recovered by volume handle.
- Upstream v1.4.32 to v1.4.34: NodeGetVolumeStats, VolumeAttributesClass
  store, per-volume writeback flags, fsGroupPolicy File.

| Image | Digest |
| --- | --- |
| `seaweedfs-csi-driver:v1.4.34-appmana.post.1` | `sha256:49fb1b43dcae792ae418249332d85d8729a5c8a9b2a4874c15aaa75c7ab7728c` |
| `seaweedfs-csi-driver:v1.4.34-appmana.post.1-linux-amd64` | `sha256:29637936a2db4b576e37abb88bb76afd442f8605b2a0197efe6db7aedbc187fc` |
| `seaweedfs-csi-driver:v1.4.34-appmana.post.1-windows-ltsc2022` | `sha256:3caad636282732a44ee4e5cb2f6b319840d3d10556c6341473e22e72b3319a39` |
| `seaweedfs-mount:v1.4.34-appmana.post.1` | `sha256:363401a46bca65ab3851ba9a6f724a7f3d6c708f3bf6b799c647cf5475a6fff1` |
| `seaweedfs-mount:v1.4.34-appmana.post.1-linux-amd64` | `sha256:c6ed7b1f75843d0e0bf566328a1a6d78fff522a69722cf1483d15cc8f476dbe0` |
| `seaweedfs-mount:v1.4.34-appmana.post.1-windows-ltsc2022` | `sha256:ca275e53af04a88292202757979417702ed51219bc2d09f9e4b6314a5df7bc57` |
