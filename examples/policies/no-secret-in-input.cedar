// no-secret-in-input.cedar
//
// Forbid any tool call whose input contains common secret patterns. The
// rule fires conservatively against the four most prevalent leaked-secret
// formats: OpenAI keys (`sk-*`), GitHub PATs (`ghp_*` and the longer
// `github_pat_*`), and `aws_secret_*` credential pair fragments.
//
// Coverage is intentionally narrow: this is a structural backstop for the
// LogScrubber, not a complete DLP solution. False positives (a tool that
// legitimately echoes a literal `sk-` substring) are an acceptable cost
// for fail-closed behaviour.
//
// Cedar schema version: v1

forbid (
    principal,
    action,
    resource
) when {
    (context.input has cmd && (
        context.input.cmd like "*sk-*" ||
        context.input.cmd like "*ghp_*" ||
        context.input.cmd like "*github_pat_*" ||
        context.input.cmd like "*aws_secret_*"
    )) ||
    (context.input has content && (
        context.input.content like "*sk-*" ||
        context.input.content like "*ghp_*" ||
        context.input.content like "*github_pat_*" ||
        context.input.content like "*aws_secret_*"
    )) ||
    (context.input has url && (
        context.input.url like "*sk-*" ||
        context.input.url like "*ghp_*" ||
        context.input.url like "*github_pat_*" ||
        context.input.url like "*aws_secret_*"
    ))
};
