# Releases

Each release is the exact images of one CI candidate (`build-images.yml`,
`candidate-<sha>-<run>-<attempt>`) promoted by `promote-release.yml`; see
"Releases" in the README. Pins name the release and the digest:
`ghcr.io/appmana/<image>:<release>[-<platform>]@sha256:<digest>`.
Earlier releases (`v1.4.12-appmana.post.*`, `v1.4.26-appmana.post.*`)
predate this file.

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
