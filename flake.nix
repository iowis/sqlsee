{
  description = "sqlsee";
  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
  outputs =
    { nixpkgs, ... }:
    let
      system = "x86_64-linux";
      pkgs = import nixpkgs {
        inherit system;
      };
    in
    {
      devShells.${system}.default =
        with pkgs;
        pkgs.mkShell {
          packages = [
            go-task
            go_1_26
          ];
          shellHook = ''
            export PATH="${pkgs.go_1_26}/bin:$PATH"
            export USER_ID="$(id -u)"
            export GROUP_ID="$(id -g)"

            export GOBIN="$PWD/.bin"
            export PATH="$GOBIN:$PATH"

            if ! [ -x "$GOBIN/gopls" ] || ! "$GOBIN/gopls" version 2>/dev/null | grep -q 'v0.23.0'; then
              go install golang.org/x/tools/gopls@v0.23.0
            fi
          '';
        };
    };
}
