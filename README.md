# bringyour


code style -
use a pointer when the value can be nil

## Using Nix Flake and Direnv for development dependencies

To get all dev dependencies (golang, node, python ...) and appropriate env variable set when changing to this repo's directory, install [direnv](https://direnv.net/) and [nix](https://nixos.org/download/).
`packages` part of [flake.nix](./flake.nix) contains list of all external tools needed to develop in this repo.
To add more dependencies, find the appropriate package in the [nix search](https://search.nixos.org/packages) and add them to the list.
