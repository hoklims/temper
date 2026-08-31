import './InboxIntro.scss'

export function InboxIntro() {
  return (
    <section className="inbox-intro" aria-labelledby="inbox-title">
      <span className="inbox-intro__eyebrow mono-label">how review works</span>
      <h1 id="inbox-title" className="inbox-intro__heading">
        Review the rules Temper learned from your sessions.
      </h1>
      <ol className="inbox-intro__steps">
        <li className="inbox-intro__step">
          <span className="inbox-intro__step-name">evidence</span>
          <span className="inbox-intro__step-desc">source excerpts when this proposal came from a session</span>
        </li>
        <li className="inbox-intro__step">
          <span className="inbox-intro__step-name">edit</span>
          <span className="inbox-intro__step-desc">adjust the rule body before deciding, if needed</span>
        </li>
        <li className="inbox-intro__step">
          <span className="inbox-intro__step-name">decide</span>
          <span className="inbox-intro__step-desc">accept writes it to the target file — reject drops it from the inbox</span>
        </li>
      </ol>
    </section>
  )
}
