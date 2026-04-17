Output demo_raw.gif

Require no-mistakes
Require git

Set Shell "bash"
Set FontSize 50
Set Width 2750
Set Height 1625
Set Padding 50
Set Theme "Catppuccin Mocha"

Env NM_DEMO "1"
Env NM_TEST_START_DAEMON "1"
Env NO_MISTAKES_NO_UPDATE_CHECK "1"
Env SHELL "/bin/bash"
Env GIT_AUTHOR_NAME "demo"
Env GIT_AUTHOR_EMAIL "demo@example.com"
Env GIT_COMMITTER_NAME "demo"
Env GIT_COMMITTER_EMAIL "demo@example.com"

# Off-screen setup: create a repo with a gate and a branch to push.
Hide
Type "export PATH=$PWD/bin:$PATH && export TMPDIR=$(mktemp -d) && export NM_HOME=$TMPDIR/.no-mistakes && UPSTREAM=$TMPDIR/upstream.git && git init --bare -b main $UPSTREAM && git clone $UPSTREAM $TMPDIR/my-repo && cd $TMPDIR/my-repo && git commit --allow-empty -m 'init' && git push origin main && git checkout -b fix/nil-check && git commit --allow-empty -m 'add nil check to handler' && no-mistakes init 2>&1 && clear"
Enter
Sleep 8s
Show

# Push through the gate, then attach to the TUI
Sleep 1s
Type "git push"
Sleep 1s
Type " no-mistakes"
Sleep 3s
Enter
Sleep 2s
Type "no-mistakes"
Sleep 3s
Enter

# Wait for the review step to stream its log and surface the approval screen.
Sleep 9s

# Linger on the approval screen so the human-in-the-loop moment is clear.
Sleep 2.5s

# Press `f` to fix the reported findings, then watch the rest of the pipeline.
Type "f"
Sleep 29s

# Let the completed state linger
Sleep 2s
