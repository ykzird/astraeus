# Example reports

Three reports produced by `analyze-astraeus-bundle.py`, kept as a reference for
what the output looks like and for reviewing changes to it.

| File | Where it came from |
| --- | --- |
| `report-binary-process.md` | a **real** bundle from a binary deployment: `./astraeus-server serve --db astra.db` run directly on an AMD host, on a machine where the VAAPI driver *is* installed |
| `report-amd-vaapi-live-session.md` | a **real** bundle from a container on the same AMD host, with `/dev/dri` mounted and a live VP9 transcoding session |
| `report-intel-nvidia-example.md` | a **synthetic** bundle shaped like the Intel + NVIDIA host this was written for; no such hardware was available to test on |

The first two are genuine output. Comparing them is the point: the same host
accepts `vaapi` natively and does not in the container, because the image ships
no `*_drv_video.so`. `report-binary-process.md` also shows the missing-log case
— a binary started from a terminal has no log history to collect.

The third is constructed from `scripts/tests/make-fixture-bundle.py` and shows
the shape an Intel + NVIDIA host will produce. It is a fixture, not measurement:
do not quote its numbers as findings about any machine.

Regenerate any of them with:

```sh
python3 scripts/analyze-astraeus-bundle.py <bundle> --out report.md
python3 scripts/analyze-astraeus-bundle.py <bundle> --label "name-I-recognise.tar.gz"
```

`--label` exists because a bundle is extracted to a temporary directory before
it is read, and the report should name the file the reader actually has.
