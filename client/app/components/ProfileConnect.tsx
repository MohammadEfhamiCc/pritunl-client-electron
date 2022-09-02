/// <reference path="../References.d.ts"/>
import * as React from "react"
import * as ProfileTypes from "../types/ProfileTypes"
import * as ProfileActions from "../actions/ProfileActions"
import * as ServiceActions from "../actions/ServiceActions"
import * as Blueprint from "@blueprintjs/core"

interface Props {
	profile: ProfileTypes.ProfileRo
	onConfirm?: () => void
}

interface State {
	disabled: boolean
	password: string
	dialog: boolean
	confirm: number
	confirming: string
}

const css = {
	box: {
		display: "inline-block"
	} as React.CSSProperties,
	button: {
		marginRight: "10px",
	} as React.CSSProperties,
	dialog: {
		width: "340px",
		position: "absolute",
	} as React.CSSProperties,
	label: {
		width: "100%",
		maxWidth: "220px",
		margin: "18px 0 0 0",
	} as React.CSSProperties,
	input: {
		width: "100%",
	} as React.CSSProperties,
}

export default class ProfileConnect extends React.Component<Props, State> {
	constructor(props: Props, context: any) {
		super(props, context)
		this.state = {
			disabled: false,
			password: "",
			dialog: false,
			confirm: 0,
			confirming: null,
		}
	}

	connect(mode: string, password: string): void {
		let prfl = this.props.profile;

		let serverPubKey = ""
		let serverBoxPubKey = ""
		if (prfl.server_public_key && (mode === "wg" || prfl.token || password)) {
			serverPubKey = prfl.server_public_key.join("\n")
			serverBoxPubKey = prfl.server_box_public_key
		}

		prfl.readData().then((data: string): void => {
			if (!data) {
				this.setState({
					...this.state,
					disabled: false,
				})
				return
			}

			let connData: ProfileTypes.ProfileData = {
				id: prfl.id,
				mode: mode,
				org_id: prfl.organization_id,
				user_id: prfl.user_id,
				server_id: prfl.server_id,
				sync_hosts: prfl.sync_hosts,
				sync_token: prfl.sync_token,
				sync_secret: prfl.sync_secret,
				username: "pritunl",
				password: password,
				dynamic_firewall: prfl.dynamic_firewall,
				server_public_key: serverPubKey,
				server_box_public_key: serverBoxPubKey,
				token_ttl: prfl.token_ttl,
				timeout: true,
				data: data,
			}

			ServiceActions.connect(connData).then((): void => {
				this.setState({
					...this.state,
					disabled: false,
				})
			})
		})
	}

	disconnect(): void {
		let prfl = this.props.profile;

		let disconnData: ProfileTypes.ProfileData = {
			id: prfl.id,
		}

		ServiceActions.disconnect(disconnData).then((): void => {
			this.setState({
				...this.state,
				disabled: false,
			})
		})
	}

	onConnect = (): void => {
		this.setState({
			...this.state,
			disabled: true,
		})
		if (this.connected()) {
			this.disconnect()
		} else {
			this.props.profile.sync().then((): void => {
				if (this.props.profile.password_mode ||
					this.props.profile.pre_connect_msg) {

					this.setState({
						...this.state,
						disabled: false,
						dialog: true,
					})
				} else {
					this.connect("ovpn", "")
				}
			})
		}
	}

	closeDialog = (): void => {
		this.setState({
			...this.state,
			dialog: false,
		})
	}

	closeDialogConfirm = (): void => {
		this.connect("ovpn", this.state.password)
		this.setState({
			...this.state,
			dialog: false,
			password: "",
		})
	}

	clearConfirm = (): void => {
		this.setState({
			...this.state,
			confirm: 0,
			confirming: null,
		})
	}

	connected = (): boolean => {
		let prfl = this.props.profile

		if (prfl.system) {
			return prfl.state
		} else {
			return !!prfl.status && prfl.status !== "disconnected"
		}
	}

	render(): JSX.Element {
		let buttonClass = ""
		let buttonLabel = ""
		if (this.connected()) {
			buttonClass = "bp3-intent-danger bp3-icon-link"
			buttonLabel = "Disconnect"
		} else {
			buttonClass = "bp3-intent-success bp3-icon-link"
			buttonLabel = "Connect"
		}

		return <div style={css.box}>
			<button
				className={"bp3-button " + buttonClass}
				style={css.button}
				type="button"
				disabled={this.state.disabled}
				onClick={this.onConnect}
			>
				{buttonLabel}
			</button>
			<Blueprint.Dialog
				title="Profile Connect"
				style={css.dialog}
				isOpen={this.state.dialog}
				usePortal={true}
				portalContainer={document.body}
				onClose={this.closeDialog}
			>
				<div className="bp3-dialog-body">
					Connecting to {this.props.profile.formattedName()}
					<div hidden={!this.props.profile.pre_connect_msg}>
						<br/>
						{this.props.profile.pre_connect_msg}
					</div>
					<label
						className="bp3-label"
						style={css.label}
					>
						Enter password:
						<input
							className="bp3-input"
							style={css.input}
							disabled={this.state.disabled}
							autoCapitalize="off"
							spellCheck={false}
							placeholder="Enter password"
							value={this.state.password}
							onChange={(evt): void => {
								this.setState({
									...this.state,
									password: evt.target.value,
								})
							}}
						/>
					</label>
				</div>
				<div className="bp3-dialog-footer">
					<div className="bp3-dialog-footer-actions">
						<button
							className="bp3-button bp3-intent-danger"
							type="button"
							onClick={this.closeDialog}
						>Cancel</button>
						<button
							className="bp3-button bp3-intent-success bp3-icon-link"
							type="button"
							disabled={this.state.disabled || this.state.password === ""}
							onClick={this.closeDialogConfirm}
						>Connect</button>
					</div>
				</div>
			</Blueprint.Dialog>
		</div>
	}
}
