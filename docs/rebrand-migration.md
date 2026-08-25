# Repository rename: Microserver to Coldstore

The application, binary, service, configuration, and Go module are already named
Coldstore. This migration only renames the GitHub repository and local checkout.

## 1. Save work and rename the GitHub repository

Before moving anything, commit unfinished work in every checkout shown by:

```bash
cd ~/Projects/Microserver
git worktree list
```

The output should list only `~/Projects/Microserver`. Remove or relocate any linked
worktrees before continuing.

Then rename the GitHub repository from `Microserver` to `coldstore`.

## 2. Rename the local checkout

```bash
cd ~/Projects
mv Microserver Coldstore
cd Coldstore
```

## 3. Update and verify the remote

```bash
git remote set-url origin https://github.com/tjmisko/coldstore.git
git fetch origin
git remote -v
git worktree list
git status
```

Any remaining use of “microserver” that describes the physical HP MicroServer host may
stay. Update old `~/Projects/Microserver` checkout paths in ignored local deployment
notes when they are next used.
