#!/usr/bin/env bash
# clusage guard rail, kept for installs that registered this script. The hook
# now lives in the clusage binary as `clusage hook run`, which works on macOS,
# Linux and Windows. Run `clusage hook install` to register the binary instead
# of this file. See `clusage help hook`.
#
# The old flags map onto the subcommands that replaced them.
case "${1:-}" in
  --install | --uninstall | --status | --interval | --project)
    sub=${1#--}
    shift
    exec clusage hook "$sub" "$@"
    ;;
esac
exec clusage hook run
