// SPDX-License-Identifier: Apache-2.0
// Shell completion scripts generated from the command tree (so they never drift from the CLI).

export interface TreeNode {
  /** Space-joined command path ("" is the root). */
  path: string;
  /** Subcommand names and flag spellings offered at this path. */
  words: string[];
}

export const SHELLS = ["bash", "zsh", "powershell"] as const;
export type Shell = (typeof SHELLS)[number];

const BIN = "nurtuoctl";

export function generateCompletion(shell: Shell, tree: readonly TreeNode[]): string {
  const paths = tree.filter((n) => n.path !== "").map((n) => n.path);
  switch (shell) {
    case "bash":
      return bash(tree, paths);
    case "zsh":
      return zsh(tree, paths);
    case "powershell":
      return powershell(tree);
  }
}

const caseList = (paths: string[]) => paths.map((p) => `"${p}"`).join("|") || '"__none__"';

function bash(tree: readonly TreeNode[], paths: string[]): string {
  const arms = tree.map((n) => `    "${n.path}") opts="${n.words.join(" ")}" ;;`).join("\n");
  return `# bash completion for ${BIN}; load with: source <(${BIN} completion bash)
_${BIN}() {
  local cur="\${COMP_WORDS[COMP_CWORD]}" path="" np w i opts=""
  for ((i = 1; i < COMP_CWORD; i++)); do
    w="\${COMP_WORDS[i]}"
    case "$w" in -*) continue ;; esac
    if [ -z "$path" ]; then np="$w"; else np="$path $w"; fi
    case "$np" in ${caseList(paths)}) path="$np" ;; esac
  done
  case "$path" in
${arms}
  esac
  COMPREPLY=($(compgen -W "$opts" -- "$cur"))
}
complete -o default -F _${BIN} ${BIN}
`;
}

function zsh(tree: readonly TreeNode[], paths: string[]): string {
  const arms = tree.map((n) => `    "${n.path}") opts=(${n.words.join(" ")}) ;;`).join("\n");
  return `#compdef ${BIN}
# zsh completion for ${BIN}; load with: source <(${BIN} completion zsh)
_${BIN}() {
  local path="" np w
  local -a opts
  for w in \${words[2,CURRENT-1]}; do
    case "$w" in -*) continue ;; esac
    if [ -z "$path" ]; then np="$w"; else np="$path $w"; fi
    case "$np" in ${caseList(paths)}) path="$np" ;; esac
  done
  case "$path" in
${arms}
  esac
  compadd -a opts
  _files
}
if [ "$funcstack[1]" = "_${BIN}" ]; then
  _${BIN} "$@"
else
  compdef _${BIN} ${BIN}
fi
`;
}

function powershell(tree: readonly TreeNode[]): string {
  const rows = tree.map((n) => `  '${n.path}' = '${n.words.join(" ")}'`).join("\n");
  return `# PowerShell completion for ${BIN}; load with: ${BIN} completion powershell | Out-String | Invoke-Expression
$script:NurtuoTree = @{
${rows}
}
Register-ArgumentCompleter -Native -CommandName ${BIN} -ScriptBlock {
  param($wordToComplete, $commandAst, $cursorPosition)
  $path = ''
  $elements = $commandAst.CommandElements | Select-Object -Skip 1
  foreach ($e in $elements) {
    $w = $e.ToString()
    if ($w.StartsWith('-') -or $w -eq $wordToComplete) { continue }
    $np = if ($path -eq '') { $w } else { "$path $w" }
    if ($script:NurtuoTree.ContainsKey($np)) { $path = $np }
  }
  $script:NurtuoTree[$path] -split ' ' | Where-Object { $_ -and $_ -like "$wordToComplete*" } |
    ForEach-Object { [System.Management.Automation.CompletionResult]::new($_, $_, 'ParameterValue', $_) }
}
`;
}
