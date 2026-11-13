# Prospectively fixed 100 ns H1 regression reference

The [owner-approved replacement pilot](../CALIBRATION-REDUCED-PACING-OWNER-APPROVAL-2026-10-30.md)
will use the complete `product.patch` in this directory: SHA-256
6e5e94f03715df42d9d9169aef5ab3272cbac0311ae5292dbbdc819c23ea2924.
It applies to the frozen server a9e8e1a710b24c749e6499a0606938252c83e136.
The resulting `connect/transport.go` SHA-256 is
69a50b2cc7d06369d3ac99f75baa147246a5b821ac8402272155f23e05626566.

Relative to the rejected one-microsecond reference, this changes only its default
to `100 * time.Nanosecond` per user-frame byte and the corresponding explanatory
comment. Pacing, cancellation, buffer ownership and original write-deadline
logic remain identical. The value is fixed before another measurement, not tuned
after another result. This patch is a calibration fixture, not a proposed product
improvement or a change to the competition source/scorer.

At publication, its actual native candidate commit, image, binary, independent
correctness qualification and newly qualified operator are pending. The old
112-test and 988481 operator results do not qualify changed code or a new ELF.
No new measurement, successful direction, hidden-seed commitment or grading
authorization is claimed. Submission G1–G6, R9, alpha, margin and deadlines stay
unchanged; the failed public pilot remains preserved and rejected.
