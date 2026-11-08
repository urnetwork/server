# The sidecar's pending migration groups link only where they may run (DESIGN.md
# §19.7, D-OPS-6): a build for WARP_ENV local or canary sets the sidecar_pending
# tag, and every other build, main's and community's among them, is a release leg
# and never does. A binary built with the tag also panics at start outside local,
# canary and tests (db_migrations_sidecar_pending.go); this keeps the tag out of a
# release build in the first place, and build/all/release_tags_test.go holds every
# Makefile to it.
#
# Each cli Makefile includes this file beside it and passes $(SIDECAR_TAGS) to go
# build, as a host Makefile another plan adds does too. run-local.sh and
# bringyourctl's local runs take the tag from local/local-env.sh's GOFLAGS.
SIDECAR_TAGS :=
ifneq ($(filter local canary,$(WARP_ENV)),)
SIDECAR_TAGS := -tags sidecar_pending
endif
